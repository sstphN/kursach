package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"1333/internal/bots"
	"1333/internal/exchanges/binance"
	"1333/internal/models"
	"1333/internal/monitor"
	"1333/persistence"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Config упрощённо
type Config struct {
	MainBotToken        string            `mapstructure:"main_bot_token"`
	AdditionalBots      map[string]string `mapstructure:"additional_bots"`
	AdditionalUsernames map[string]string `mapstructure:"additional_usernames"`
	BinanceAPI          struct {
		Intraday struct {
			APIKey    string `mapstructure:"api_key"`
			APISecret string `mapstructure:"api_secret"`
		} `mapstructure:"intraday"`
		Scalp struct {
			APIKey    string `mapstructure:"api_key"`
			APISecret string `mapstructure:"api_secret"`
		} `mapstructure:"scalp"`
		Spot struct {
			APIKey    string `mapstructure:"api_key"`
			APISecret string `mapstructure:"api_secret"`
		} `mapstructure:"spot"`
	} `mapstructure:"binance_api"`
}

// упрощённая loadConfig
func loadConfig(path string) (*Config, error) {
	viper.SetConfigFile(path)
	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func main() {
	log.SetLevel(log.InfoLevel)

	// 1) Читаем config
	cfg, err := loadConfig("configs/config.json")
	if err != nil {
		log.Fatalf("Ошибка loadConfig: %v", err)
	}

	// 2) Binance-клиенты
	binanceClients := map[string]*binance.Client{
		"intraday": binance.NewClient(cfg.BinanceAPI.Intraday.APIKey, cfg.BinanceAPI.Intraday.APISecret),
		"scalp":    binance.NewClient(cfg.BinanceAPI.Scalp.APIKey, cfg.BinanceAPI.Scalp.APISecret),
	}
	for mode, c := range binanceClients {
		if c == nil {
			log.Errorf("Не удалось создать Binance клиент для %s", mode)
		}
	}
	// Spot
	spotCl := binance.NewSpotClient(cfg.BinanceAPI.Spot.APIKey, cfg.BinanceAPI.Spot.APISecret)

	// 3) Подключаемся к Postgres
	dsn := "host=localhost port=5432 user=postgres password=666 dbname=mydb2 sslmode=disable"
	userStore, err := persistence.NewUserStorePostgres(dsn)
	if err != nil {
		log.Fatalf("NewUserStorePostgres: %v", err)
	}
	if err := userStore.Load(); err != nil {
		log.Errorf("Load UserStore err=%v", err)
	}

	// Хранилище метрик
	userMetricsStore, err := persistence.NewUserMetricsStorePostgres(dsn)
	if err != nil {
		log.Fatalf("NewUserMetricsStorePostgres: %v", err)
	}
	if err := userMetricsStore.LoadAll(context.Background()); err != nil {
		log.Errorf("userMetricsStore.LoadAll: %v", err)
	}

	// 4) BotManager
	manager, err := bots.NewBotManager(
		cfg.MainBotToken,
		cfg.AdditionalBots,
		cfg.AdditionalUsernames,
	)
	if err != nil {
		log.Fatalf("NewBotManager: %v", err)
	}

	// 5) Привяжем колбэк OnSettingsFn к главному боту
	if mainBot, ok := manager.Bots["main"]; ok {
		mainBot.OnSettingsFn = func(userID int64, s models.UserSettings) {
			ctx := context.Background()
			var metricID models.MetricID
			var threshold float64 = 0
			var timeframe string = s.TimeFrame
			var allSymbols []string

			// Получаем список символов в зависимости от Mode
			switch s.Mode {
			case "scalp":
				// качаем USDM futures для scalp
				metricID = models.MetricScalpPD // по умолчанию
				if s.SomeIntradayMetric == "" {

					threshold = s.ChangeThreshold
					metricID = models.MetricScalpPD
				}
				// качаем все символы
				if binanceClients["scalp"] != nil {
					syms, _ := binanceClients["scalp"].NewExchangeInfoService(ctx)
					allSymbols = syms
				}

			case "intraday":
				if s.MonitorOI {
					metricID = models.MetricIntradayOI
					threshold = s.OIThreshold
				}
				if s.SomeIntradayMetric == "pumps_dumps" {
					metricID = models.MetricIntradayPD
					threshold = s.SomeIntradayThreshold
				}
				if s.MonitorDelta {
					metricID = models.MetricIntradayDelta
					timeframe = s.DeltaInterval
					threshold = 0 // дельта-логика у нас своя
				}
				// качаем все символы
				if binanceClients["intraday"] != nil {
					syms, _ := binanceClients["intraday"].NewExchangeInfoService(ctx)
					allSymbols = syms
				}

			case "spot":
				// если userSettings.MonitorSpot => metric=spot_volume
				metricID = models.MetricSpotVolume
				threshold = s.SpotVolumeMultiplier
				// Получить &laquo;все символы&raquo; на Spot можно отдельным методом,
				// но в коде у нас нет готового. Часто делается через binance.NewClientSpotExchangeInfoService().
				// Для примера возьмём те же USDM (чтобы было хоть что-то).
				if binanceClients["intraday"] != nil {
					syms, _ := binanceClients["intraday"].NewExchangeInfoService(ctx)
					allSymbols = syms
				}
			}

			// Собираем MetricSettings
			ms := models.MetricSettings{
				Enabled:   true,
				TargetBot: s.TargetBot,
				TimeFrame: timeframe,
				Threshold: threshold,
				Symbols:   allSymbols,
			}
			// Сохраняем в user_metrics
			if err := userMetricsStore.SetMetricSettings(ctx, userID, metricID, ms); err != nil {
				log.Errorf("OnSettingsFn: SetMetricSettings err=%v", err)
			} else {
				log.Infof("OnSettingsFn: пользователь %d => metric=%s сохранено", userID, metricID)
			}
		}
	}

	// 6) Запускаем WebSocket потоки
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if c := binanceClients["intraday"]; c != nil {
		if err := c.StartWebsocketStreams(ctx); err != nil {
			log.Errorf("Intraday WS err=%v", err)
		}
	}
	if c := binanceClients["scalp"]; c != nil {
		if err := c.StartWebsocketStreams(ctx); err != nil {
			log.Errorf("Scalp WS err=%v", err)
		}
	}
	if spotCl != nil {
		// Пример: берём те же symbols от intraday
		syms, _ := binanceClients["intraday"].NewExchangeInfoService(ctx)
		spotCl.StartWebsocket(ctx, syms, "1m")
	}

	// 7) Запускаем GlobalMonitor (пакет monitor)
	globalMon := monitor.NewGlobalMonitor(
		userStore,
		userMetricsStore,
		binanceClients,
		spotCl,
		manager,
	)
	globalMon.Start(ctx)

	// 8) Запускаем чтение апдейтов главного бота
	if mainBot, ok := manager.Bots["main"]; ok {
		go func() {
			u := tgbotapi.NewUpdate(0)
			u.Timeout = 60
			updates := mainBot.BotAPI.GetUpdatesChan(u)
			for upd := range updates {
				mainBot.HandleUpdate(upd)
			}
		}()
	}

	// 9) Ждём сигнал
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, os.Interrupt, syscall.SIGTERM)
	<-sigC

	cancel()
	globalMon.Wait()
	log.Info("Завершено.")
}
