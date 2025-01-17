package monitor

import (
	"context"
	"fmt"
	"math"

	"strings"
	"sync"
	"time"

	"1333/internal/bots"
	"1333/internal/exchanges/binance"
	"1333/internal/models"
	"1333/internal/utils"
	"1333/persistence"

	log "github.com/sirupsen/logrus"
)

type OIRecord struct {
	Timestamp time.Time
	OI        float64
}

// GlobalMonitor — наш основной монитор
type GlobalMonitor struct {
	store          *persistence.UserStore
	userMetrics    *persistence.UserMetricsStore
	binanceClients map[string]*binance.Client
	SpotClient     *binance.SpotClient
	botManager     *bots.BotManager

	Symbols         []string
	tickerInterval  time.Duration
	stopChan        chan struct{}
	wg              sync.WaitGroup
	alertCooldown   time.Duration
	lastAlertTimes  map[int64]map[models.MetricID]time.Time
	lastAlertMutex  sync.Mutex
	oiHistory       map[string][]OIRecord
	oiHistoryMutex  sync.RWMutex
	symbolUsers     map[string][]int64
	symbolMutex     sync.RWMutex
	refreshInterval time.Duration
}

// NewGlobalMonitor — конструктор
func NewGlobalMonitor(
	store *persistence.UserStore,
	userMetrics *persistence.UserMetricsStore,
	binanceClients map[string]*binance.Client,
	spotCl *binance.SpotClient,
	botManager *bots.BotManager,
) *GlobalMonitor {
	return &GlobalMonitor{
		store:           store,
		userMetrics:     userMetrics,
		binanceClients:  binanceClients,
		SpotClient:      spotCl,
		botManager:      botManager,
		tickerInterval:  1 * time.Minute,
		stopChan:        make(chan struct{}),
		wg:              sync.WaitGroup{},
		alertCooldown:   20 * time.Second, // как в старом коде
		lastAlertTimes:  make(map[int64]map[models.MetricID]time.Time),
		oiHistory:       make(map[string][]OIRecord),
		symbolUsers:     make(map[string][]int64),
		refreshInterval: 10 * time.Minute,
	}
}

// Start — запускаем мониторинг
func (gm *GlobalMonitor) Start(ctx context.Context) {
	gm.wg.Add(1)
	go gm.run(ctx)

	gm.wg.Add(1)
	go gm.autoRefreshSymbols(ctx)

	gm.wg.Add(1)
	go gm.runDeltaMonitoring(ctx)
}

func (gm *GlobalMonitor) run(ctx context.Context) {
	defer gm.wg.Done()

	// Изначально собираем OI
	gm.initializeOIHistory(ctx)

	ticker := time.NewTicker(gm.tickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("GlobalMonitor: завершение.")
			return
		case <-ticker.C:
			gm.process(ctx)
		}
	}
}

func (gm *GlobalMonitor) autoRefreshSymbols(ctx context.Context) {
	defer gm.wg.Done()
	ticker := time.NewTicker(gm.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("GlobalMonitor: Завершение автообновления символов.")
			return
		case <-ticker.C:
			log.Println("GlobalMonitor: Автообновление списка символов...")
			gm.InitializeSymbolUsers(ctx)
		}
	}
}

// InitializeSymbolUsers — заполняем symbolUsers
func (gm *GlobalMonitor) InitializeSymbolUsers(ctx context.Context) {
	users := gm.store.All()
	symbolUserMap := make(map[string][]int64)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for uid, user := range users {
		wg.Add(1)
		go func(uid int64, userSettings *models.UserSettings) {
			defer wg.Done()

			for _, sym := range userSettings.Symbols {
				client, ok := gm.binanceClients[userSettings.Mode]
				if !ok {
					continue
				}
				isTrading, err := client.IsSymbolTrading(ctx, sym)
				if err != nil {
					log.Printf("Ошибка проверки статуса %s для %d: %v", sym, uid, err)
					continue
				}
				if isTrading {
					mu.Lock()
					symbolUserMap[sym] = append(symbolUserMap[sym], uid)
					mu.Unlock()
				}
			}
		}(uid, user)
	}
	wg.Wait()

	gm.symbolMutex.Lock()
	gm.symbolUsers = symbolUserMap
	gm.symbolMutex.Unlock()
	log.Println("GlobalMonitor: symbolUsers инициализированы.")
}

// initializeOIHistory — собираем OI один раз при запуске
func (gm *GlobalMonitor) initializeOIHistory(ctx context.Context) {
	log.Println("GlobalMonitor: init OI history.")

	// Смотрим все символы
	gm.symbolMutex.RLock()
	symbols := make([]string, 0, len(gm.symbolUsers))
	for sym := range gm.symbolUsers {
		symbols = append(symbols, sym)
	}
	gm.symbolMutex.RUnlock()

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, sym := range symbols {
		for mode, client := range gm.binanceClients {
			wg.Add(1)
			go func(sym, mode string, c *binance.Client) {
				defer wg.Done()
				oi, err := c.GetOpenInterest(ctx, sym)
				if err != nil {
					log.Printf("initOIHistory: Ошибка OI %s (%s): %v", sym, mode, err)
					return
				}
				record := OIRecord{
					Timestamp: time.Now(),
					OI:        oi,
				}
				mu.Lock()
				gm.oiHistory[sym] = append(gm.oiHistory[sym], record)
				gm.trimOldOI(sym)
				mu.Unlock()
			}(sym, mode, client)
		}
	}
	wg.Wait()
	log.Println("GlobalMonitor: OI history инициализирована.")
}

// process — вызывается каждую минуту
func (gm *GlobalMonitor) process(ctx context.Context) {
	log.Println("GlobalMonitor: начало цикла мониторинга.")

	// Собираем текущий OI (append + обрезка) каждый цикл
	gm.collectCurrentOI(ctx)

	// Достаём все метрики
	allConfigs := gm.userMetrics.AllConfigs()
	for userID, uc := range allConfigs {
		for metricID, ms := range uc.Metrics {
			if !ms.Enabled || ms.TargetBot == "" {
				continue
			}
			if !gm.canSendAlert(userID, metricID) {
				continue
			}

			switch metricID {
			case models.MetricScalpPD:
				scalpClient := gm.binanceClients["scalp"]
				if scalpClient == nil {
					continue
				}
				gm.handleScalpPumpsDumps(ctx, userID, ms, scalpClient)

			case models.MetricIntradayOI:
				intraClient := gm.binanceClients["intraday"]
				if intraClient == nil {
					continue
				}
				gm.handleIntradayOI(ctx, userID, ms, intraClient)

			case models.MetricIntradayPD:
				intraClient := gm.binanceClients["intraday"]
				if intraClient == nil {
					continue
				}
				gm.handleIntradayPumpsDumps(ctx, userID, ms, intraClient)

			case models.MetricSpotVolume:
				// Логика "в N раз больше среднего"
				if gm.SpotClient == nil {
					continue
				}
				gm.handleSpotVolumeMultiplier(ctx, userID, ms)

			case models.MetricIntradayDelta:
				// обрабатывается runDeltaMonitoring
			}
		}
	}

	log.Println("GlobalMonitor: цикл мониторинга завершён.")
}

// collectCurrentOI — собираем OI в oiHistory для всех символов
func (gm *GlobalMonitor) collectCurrentOI(ctx context.Context) {
	gm.symbolMutex.RLock()
	symbols := make([]string, 0, len(gm.symbolUsers))
	for sym := range gm.symbolUsers {
		symbols = append(symbols, sym)
	}
	gm.symbolMutex.RUnlock()

	var wg sync.WaitGroup

	for _, sym := range symbols {
		for mode, client := range gm.binanceClients {
			wg.Add(1)
			go func(sym, mode string, c *binance.Client) {
				defer wg.Done()
				oi, err := c.GetOpenInterest(ctx, sym)
				if err != nil {
					return
				}
				gm.oiHistoryMutex.Lock()
				gm.oiHistory[sym] = append(gm.oiHistory[sym], OIRecord{
					Timestamp: time.Now(),
					OI:        oi,
				})
				gm.trimOldOI(sym)
				gm.oiHistoryMutex.Unlock()
			}(sym, mode, client)
		}
	}
	wg.Wait()
}

// trimOldOI — обрезаем всё старше 30 минут
func (gm *GlobalMonitor) trimOldOI(sym string) {
	cutoff := time.Now().Add(-30 * time.Minute)
	arr := gm.oiHistory[sym]
	i := 0
	for ; i < len(arr); i++ {
		if arr[i].Timestamp.After(cutoff) {
			break
		}
	}
	gm.oiHistory[sym] = arr[i:]
}

// handleScalpPumpsDumps — пампы/дампы для scalp
func (gm *GlobalMonitor) handleScalpPumpsDumps(ctx context.Context, userID int64, ms *models.MetricSettings, client *binance.Client) {
	for _, sym := range ms.Symbols {
		prevClose, currClose, err := client.GetChangePercent(ctx, sym, ms.TimeFrame)
		if err != nil {
			continue
		}
		cp := ((currClose - prevClose) / prevClose) * 100
		if math.Abs(cp) >= ms.Threshold {
			indicator := "🟥 Dump"
			if cp > 0 {
				indicator = "🟩 Pump"
			}
			priceFmt := utils.FormatPriceWithComma(currClose, 4)
			msg := fmt.Sprintf("%s: `%s`\npriceChange: %.2f%%\nТекущая цена: %s USDT",
				indicator, sym, cp, priceFmt)

			gm.botManager.SendToBot(ms.TargetBot, userID, msg)
			gm.fixAlertTime(userID, models.MetricScalpPD)
		}
	}
}

// handleIntradayOI — ищем oi15m, oi30m, сравниваем с текущим OI
func (gm *GlobalMonitor) handleIntradayOI(ctx context.Context, userID int64, ms *models.MetricSettings, client *binance.Client) {
	// Для каждого символа сравним текущее OI и oi15m, oi30m
	for _, sym := range ms.Symbols {
		// 1) Получим текущий OI
		currentOI, err := client.GetOpenInterest(ctx, sym)
		if err != nil {
			continue
		}

		// 2) Ищем oi15m и oi30m по времени
		var oi15m, oi30m float64
		now := time.Now()

		gm.oiHistoryMutex.RLock()
		history := gm.oiHistory[sym]
		for _, rec := range history {
			// oi15m: ищем запись в промежутке [now-16m, now-14m]
			if rec.Timestamp.Before(now.Add(-14*time.Minute)) &&
				rec.Timestamp.After(now.Add(-16*time.Minute)) {
				oi15m = rec.OI
			}
			// oi30m: ищем запись в промежутке [now-31m, now-29m]
			if rec.Timestamp.Before(now.Add(-29*time.Minute)) &&
				rec.Timestamp.After(now.Add(-31*time.Minute)) {
				oi30m = rec.OI
			}
		}
		gm.oiHistoryMutex.RUnlock()

		// 3) Если хотя бы один из них != 0, считаем изменения
		var msgBuilder strings.Builder
		shouldAlert := false

		if oi15m != 0 && hasSignificantChange(currentOI, oi15m, ms.Threshold) {
			change15m := ((currentOI - oi15m) / oi15m) * 100
			msgBuilder.WriteString(fmt.Sprintf("OI Change (15m): %.2f%%\n", change15m))
			shouldAlert = true
		}
		if oi30m != 0 && hasSignificantChange(currentOI, oi30m, ms.Threshold) {
			change30m := ((currentOI - oi30m) / oi30m) * 100
			msgBuilder.WriteString(fmt.Sprintf("OI Change (30m): %.2f%%\n", change30m))
			shouldAlert = true
		}

		if shouldAlert {
			// Дополним сообщением
			msg := fmt.Sprintf("🎰 OI Alert\n`%s`\nТекущий OI=%.2f\n%s", sym, currentOI, msgBuilder.String())
			gm.botManager.SendToBot(ms.TargetBot, userID, msg)
			gm.fixAlertTime(userID, models.MetricIntradayOI)
		}
	}
}

// handleIntradayPumpsDumps — пример
func (gm *GlobalMonitor) handleIntradayPumpsDumps(ctx context.Context, userID int64, ms *models.MetricSettings, client *binance.Client) {
	for _, sym := range ms.Symbols {
		prevClose, currClose, err := client.GetChangePercent(ctx, sym, ms.TimeFrame)
		if err != nil {
			continue
		}
		cp := ((currClose - prevClose) / prevClose) * 100
		if math.Abs(cp) >= ms.Threshold {
			indic := "🟥 Dump"
			if cp > 0 {
				indic = "🟩 Pump"
			}
			currFmt := utils.FormatPriceWithComma(currClose, 4)
			msg := fmt.Sprintf("📈 Intraday %s: `%s`\n&Delta;=%.2f%%\nPrice= %s USDT",
				indic, sym, cp, currFmt)
			gm.botManager.SendToBot(ms.TargetBot, userID, msg)
			gm.fixAlertTime(userID, models.MetricIntradayPD)
		}
	}
}

// handleSpotVolume
func (gm *GlobalMonitor) handleSpotVolumeMultiplier(ctx context.Context, userID int64, ms *models.MetricSettings) {
	for _, sym := range ms.Symbols {
		currVol, err1 := gm.SpotClient.GetTradingVolumeUSD(ctx, sym, ms.TimeFrame)
		avgVol, err2 := gm.SpotClient.GetAverageVolume(ctx, sym, ms.TimeFrame, 15) // 15 свечей
		if err1 != nil || err2 != nil {
			continue
		}
		if avgVol == 0 {
			continue
		}
		if currVol >= ms.Threshold*avgVol {
			if gm.canSendAlert(userID, models.MetricSpotVolume) {
				price, err := gm.binanceClients["intraday"].GetCurrentPrice(ctx, sym)
				if err != nil {
					log.Errorf("Ошибка получения текущей цены для %s: %v", sym, err)
					continue
				}
				msg := fmt.Sprintf("💰 Spot Volume Alert\n`%s`\nОбъём: %.2f (x%.2f от среднего %.2f)\nЦена: %.2f USDT",
					sym, currVol, ms.Threshold, avgVol, price)
				gm.botManager.SendToBot(ms.TargetBot, userID, msg)
				gm.fixAlertTime(userID, models.MetricSpotVolume)
			}
		}
	}
}

// runDeltaMonitoring — дельта
func (gm *GlobalMonitor) runDeltaMonitoring(ctx context.Context) {
	defer gm.wg.Done()
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("runDeltaMonitoring: завершение (ctx done)")
			return
		case <-ticker.C:
			log.Info("runDeltaMonitoring: сбор дельты...")

			allConfigs := gm.userMetrics.AllConfigs()
			for userID, uc := range allConfigs {
				for metricID, ms := range uc.Metrics {
					if metricID != models.MetricIntradayDelta {
						continue
					}
					if !ms.Enabled || ms.TargetBot == "" {
						continue
					}
					client := gm.binanceClients["intraday"]
					if client == nil {
						continue
					}
					deltaAgg := client.DeltaAggregator()
					if deltaAgg == nil {
						log.Warn("DeltaAggregator is nil for intraday")
						continue
					}
					slots, err := binance.ParseDeltaSlots(ms.TimeFrame)
					if err != nil {
						log.Errorf("Wrong delta interval: %s", ms.TimeFrame)
						continue
					}
					// 1) Получаем Top-5 по дельте
					topList, err := deltaAgg.GetTopDeltaSymbols(ctx, client, slots, 5, 10)
					if err != nil {
						log.Errorf("GetTopDeltaSymbols err: %v", err)
						return
					}
					// Дополняем текущую цену:
					for i := range topList {
						curPrice, errCP := client.GetCurrentPrice(ctx, topList[i].Symbol)
						if errCP == nil {
							topList[i].CurrentPrice = curPrice
						} else {
							log.Warnf("Не удалось получить текущую цену для %s: %v", topList[i].Symbol, errCP)
						}
					}
					// 3) Формируем сообщение
					text := formatDeltaAlertMessage(topList, ms.TimeFrame)
					if gm.canSendAlert(userID, models.MetricIntradayDelta) {
						gm.botManager.SendToBot(ms.TargetBot, userID, text)
						gm.fixAlertTime(userID, models.MetricIntradayDelta)
					}
				}
			}
			log.Info("runDeltaMonitoring: завершено.")
		}
	}
}

// canSendAlert — кулдаун
func (gm *GlobalMonitor) canSendAlert(userID int64, metricID models.MetricID) bool {
	gm.lastAlertMutex.Lock()
	defer gm.lastAlertMutex.Unlock()

	if gm.lastAlertTimes[userID] == nil {
		gm.lastAlertTimes[userID] = make(map[models.MetricID]time.Time)
	}
	lastTime, exists := gm.lastAlertTimes[userID][metricID]
	if !exists || time.Since(lastTime) > gm.alertCooldown {
		return true
	}
	return false
}

// fixAlertTime
func (gm *GlobalMonitor) fixAlertTime(userID int64, metricID models.MetricID) {
	gm.lastAlertMutex.Lock()
	defer gm.lastAlertMutex.Unlock()

	if gm.lastAlertTimes[userID] == nil {
		gm.lastAlertTimes[userID] = make(map[models.MetricID]time.Time)
	}
	gm.lastAlertTimes[userID][metricID] = time.Now()
}

// hasSignificantChange — вспомогательная функция
func hasSignificantChange(current, previous, threshold float64) bool {
	if previous == 0 {
		return false
	}
	change := ((current - previous) / previous) * 100
	return math.Abs(change) >= threshold
}

// formatDeltaAlertMessage — дельта
func formatDeltaAlertMessage(deltas []binance.SymbolDelta, interval string) string {
	if len(deltas) == 0 {
		return fmt.Sprintf("📊🔫 *Top 5 Buying coins on Binance Futures in the last %s*\n\nНет данных.", interval)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊🔫 *Top 5 Buying coins on Binance Futures in the last %s*\n\n", interval))

	for _, d := range deltas {
		// Красивый формат Buy/Sell
		buyStr := utils.FormatVolume(d.BuyVolume)
		sellStr := utils.FormatVolume(d.SellVolume)
		priceStr := utils.FormatPriceWithComma(d.CurrentPrice, 4) // или "%.2f"

		// Собираем текст
		sb.WriteString(fmt.Sprintf(
			"*%s*\nBuy: %s, Sell: %s\nCurrent Price: %s USDT\n\n",
			d.Symbol, buyStr, sellStr, priceStr,
		))
	}

	return sb.String()
}

func (gm *GlobalMonitor) Wait() {
	gm.wg.Wait()
}

// parseMinutes
func parseMinutes(tf string) (int, error) {
	switch tf {
	case "1m":
		return 1, nil
	case "3m":
		return 3, nil
	case "5m":
		return 5, nil
	case "15m":
		return 15, nil
	case "30m":
		return 30, nil
	case "1h":
		return 60, nil
	case "4h":
		return 240, nil
	case "12h":
		return 720, nil
	case "24h", "1d":
		return 1440, nil
	}
	return 0, fmt.Errorf("unknown tf %s", tf)
}
