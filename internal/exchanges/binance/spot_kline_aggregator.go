package binance

import (
	"strconv"
	"sync"

	"github.com/adshao/go-binance/v2"
	log "github.com/sirupsen/logrus"
)

// WsSpotKlineAggregator собирает последние N закрытых свечей
type WsSpotKlineAggregator struct {
	mu      sync.RWMutex
	volumes map[string][]float64
	maxLen  int
}

func NewWsSpotKlineAggregator() *WsSpotKlineAggregator {
	return &WsSpotKlineAggregator{
		volumes: make(map[string][]float64),
		maxLen:  10, // Храним 10 последних свечей
	}
}

func (w *WsSpotKlineAggregator) handleKline(e *binance.WsKlineEvent) {
	if !e.Kline.IsFinal {
		// Игнорируем незакрытую свечу
		return
	}

	vol, err := strconv.ParseFloat(e.Kline.Volume, 64)
	if err != nil {
		log.Errorf("WsSpotKlineAggregator: parse volume err=%v", err)
		return
	}
	sym := e.Symbol

	w.mu.Lock()
	arr := w.volumes[sym]
	arr = append(arr, vol)
	if len(arr) > w.maxLen {
		arr = arr[len(arr)-w.maxLen:]
	}
	w.volumes[sym] = arr
	w.mu.Unlock()
}

func (w *WsSpotKlineAggregator) GetLastVolume(symbol string) float64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	arr := w.volumes[symbol]
	if len(arr) == 0 {
		return 0
	}
	return arr[len(arr)-1]
}

func (w *WsSpotKlineAggregator) GetSum10Volumes(symbol string) float64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	arr := w.volumes[symbol]
	var sum float64
	for _, v := range arr {
		sum += v
	}
	return sum
}

func (w *WsSpotKlineAggregator) GetAverageVolume(symbol string) float64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	arr := w.volumes[symbol]
	if len(arr) == 0 {
		return 0
	}
	var sum float64
	for _, v := range arr {
		sum += v
	}
	return sum / float64(len(arr))
}
