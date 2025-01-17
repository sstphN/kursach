package binance

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ringEntry хранит BuyVol / SellVol за один 15-минутный интервал
type ringEntry struct {
	BuyVol  float64
	SellVol float64
}

// ringBuffer — кольцо из totalSlots слотов
type ringBuffer struct {
	entries      []ringEntry
	currentIndex int
	startTime    time.Time
}

// DeltaAggregator хранит buy/sell объёмы для множества символов
type DeltaAggregator struct {
	mu             sync.RWMutex
	buffers        map[string]*ringBuffer
	totalSlots     int
	interval       time.Duration
	rotationTicker *time.Ticker
	ctxCancel      context.CancelFunc
}

// NewDeltaAggregator создаёт агрегатор с кольцевым буфером
func NewDeltaAggregator(interval time.Duration, totalSlots int) *DeltaAggregator {
	return &DeltaAggregator{
		buffers:    make(map[string]*ringBuffer),
		totalSlots: totalSlots,
		interval:   interval,
	}
}

// Start запускает ротацию раз в interval (15m)
func (da *DeltaAggregator) Start(ctx context.Context) {
	ctx2, cancel := context.WithCancel(ctx)
	da.ctxCancel = cancel

	da.rotationTicker = time.NewTicker(da.interval)
	go func() {
		for {
			select {
			case <-ctx2.Done():
				return
			case <-da.rotationTicker.C:
				da.rotateSlot()
			}
		}
	}()
}

func (da *DeltaAggregator) Stop() {
	if da.ctxCancel != nil {
		da.ctxCancel()
	}
	if da.rotationTicker != nil {
		da.rotationTicker.Stop()
	}
}

// AddTrade — вызывается на каждый aggTrade
func (da *DeltaAggregator) AddTrade(symbol string, isBuy bool, qty float64) {
	da.mu.Lock()
	defer da.mu.Unlock()

	rb, ok := da.buffers[symbol]
	if !ok {
		rb = &ringBuffer{
			entries:   make([]ringEntry, da.totalSlots),
			startTime: time.Now(),
		}
		da.buffers[symbol] = rb
	}
	if isBuy {
		rb.entries[rb.currentIndex].BuyVol += qty
	} else {
		rb.entries[rb.currentIndex].SellVol += qty
	}
}

// rotateSlot — раз в 15 минут
func (da *DeltaAggregator) rotateSlot() {
	da.mu.Lock()
	defer da.mu.Unlock()

	now := time.Now()
	for sym, rb := range da.buffers {
		rb.currentIndex = (rb.currentIndex + 1) % da.totalSlots
		// Обнуляем новый слот
		rb.entries[rb.currentIndex] = ringEntry{}
		rb.startTime = now
		log.Debugf("DeltaAggregator: rotateSlot for %s, new index=%d", sym, rb.currentIndex)
	}
	log.Info("DeltaAggregator: all slots rotated (15m).")
}

// GetDeltaSummation собирает BuyVol / SellVol за N слотов
func (da *DeltaAggregator) GetDeltaSummation(symbol string, slots int) (float64, float64) {
	da.mu.RLock()
	defer da.mu.RUnlock()

	rb, ok := da.buffers[symbol]
	if !ok {
		return 0, 0
	}
	if slots > da.totalSlots {
		slots = da.totalSlots
	}
	buySum := 0.0
	sellSum := 0.0

	idx := rb.currentIndex
	for i := 0; i < slots; i++ {
		buySum += rb.entries[idx].BuyVol
		sellSum += rb.entries[idx].SellVol
		idx = (idx - 1 + da.totalSlots) % da.totalSlots
	}
	return buySum, sellSum
}

// GetTopDeltaSymbols возвращает список, отсортированный по убыванию дельты
func (da *DeltaAggregator) GetTopDeltaSymbols(
	ctx context.Context,
	client *Client,
	slots int,
	topN int,
	min24hVolume float64,
) ([]SymbolDelta, error) {

	da.mu.RLock()
	allSymbols := make([]string, 0, len(da.buffers))
	for s := range da.buffers {
		allSymbols = append(allSymbols, s)
	}
	da.mu.RUnlock()

	var result []SymbolDelta
	var filtered []string

	// Фильтруем символы по 24h объёму
	for _, sym := range allSymbols {
		vol24, err := client.Get24hVolumeCached(ctx, sym)
		if err != nil {
			log.Errorf("GetTopDeltaSymbols: skip %s, 24hVolume err=%v", sym, err)
			continue
		}
		if vol24 >= min24hVolume {
			filtered = append(filtered, sym)
		}
	}

	for _, sym := range filtered {
		buyVol, sellVol := da.GetDeltaSummation(sym, slots)
		delta := buyVol - sellVol

		dailyVol, err := client.Get24hVolumeCached(ctx, sym)
		if err != nil {
			dailyVol = 0
		}
		var ratio float64
		if dailyVol > 0 {
			ratio = (delta / dailyVol) * 100.0
		}

		sd := SymbolDelta{
			Symbol:             sym,
			BuyVolume:          buyVol,
			SellVolume:         sellVol,
			Delta:              delta,
			RatioToDailyVolume: ratio,
		}
		result = append(result, sd)
	}

	// Сортировка по убыванию дельты
	sort.Slice(result, func(i, j int) bool {
		return result[i].Delta > result[j].Delta
	})

	if len(result) > topN {
		result = result[:topN]
	}
	return result, nil
}

// ParseDeltaSlots позволяет перевести "15m" => 1 слот, "1h" => 4, "4h" => 16, "24h" => 96 и т.д.
func ParseDeltaSlots(interval string) (int, error) {
	switch interval {
	case "15m":
		return 1, nil
	case "1h":
		return 4, nil
	case "4h":
		return 16, nil
	case "24h":
		return 96, nil
	}
	return 1, fmt.Errorf("unknown interval: %s", interval)
}
