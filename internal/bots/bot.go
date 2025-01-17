package bots

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"1333/internal/exchanges/binance"
	"1333/internal/models"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/time/rate"
)

// userSession хранит текущие шаги/состояния диалога бота
type userSession struct {
	MessageID int
	ChatID    int64
	States    []string
}

// Bot инкапсулирует логику телеграм-бота (FSM + хранение Users).
// ВАЖНО: вместо bots.UserSettings мы используем models.UserSettings
type Bot struct {
	BotAPI       *tgbotapi.BotAPI
	Users        map[int64]*models.UserSettings
	Mu           sync.Mutex
	OnSettingsFn func(userID int64, s models.UserSettings)

	ManagerRef   *BotManager
	UserSessions map[int64]*userSession
	SessionMu    sync.RWMutex

	rateLimiters map[int64]*rate.Limiter
	rateMu       sync.Mutex

	userLimiters  map[int64]*userLimiters
	BinanceClient *binance.Client
}

type userLimiters struct {
	mu       sync.Mutex
	limiters map[OperationType]*rate.Limiter
}

type OperationType string

const (
	OpLight OperationType = "light"
	OpHeavy OperationType = "heavy"
)

// NewBot создаём новый Bot
func NewBot(token string) (*Bot, error) {
	botAPI, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	return &Bot{
		BotAPI:       botAPI,
		Users:        make(map[int64]*models.UserSettings),
		UserSessions: make(map[int64]*userSession),
		rateLimiters: make(map[int64]*rate.Limiter),
	}, nil
}

// getRateLimiter — ограничивает кол-во действий пользователя
func (b *Bot) getRateLimiter(userID int64) *rate.Limiter {
	b.rateMu.Lock()
	defer b.rateMu.Unlock()

	limiter, exists := b.rateLimiters[userID]
	if !exists {
		// 3 запрос/сек, буфер 5
		limiter = rate.NewLimiter(3, 5)
		b.rateLimiters[userID] = limiter
	}
	return limiter
}

// classifyOperation — пока не используется, но оставим
func classifyOperation(update tgbotapi.Update) OperationType {
	if update.CallbackQuery != nil {
		data := update.CallbackQuery.Data
		if strings.HasPrefix(data, "set_delta_interval:") {
			return OpHeavy
		}
	}
	return OpLight
}

// HandleUpdate — главный вход в бота (Message или CallbackQuery)
func (b *Bot) HandleUpdate(update tgbotapi.Update) {
	var userID int64
	var callbackID string

	if update.Message != nil {
		userID = update.Message.Chat.ID
	} else if update.CallbackQuery != nil {
		userID = update.CallbackQuery.Message.Chat.ID
		callbackID = update.CallbackQuery.ID
	} else {
		return
	}

	// rate-limit
	limiter := b.getRateLimiter(userID)
	if !limiter.Allow() {
		if update.CallbackQuery != nil {
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, "Слишком много запросов. Подождите..."))
		} else {
			msg := tgbotapi.NewMessage(userID, "Слишком много запросов. Пожалуйста, подождите...")
			b.BotAPI.Send(msg)
		}
		return
	}

	// Обработка команд (/start, /help)
	if update.Message != nil && update.Message.IsCommand() {
		switch strings.ToLower(update.Message.Command()) {
		case "start":
			b.startCommand(userID, update.Message.From.FirstName)
		case "help":
			b.sendHelp(userID)
		default:
			b.sendUnknown(userID)
		}
		return
	}

	// Если пришло текстовое сообщение без команды
	if update.Message != nil {
		b.sendUnknown(userID)
		return
	}

	// Иначе CallbackQuery
	if update.CallbackQuery != nil {
		b.handleCallbackQuery(update.CallbackQuery)
	}
}

// handleCallbackQuery разбирает нажатия кнопок
func (b *Bot) handleCallbackQuery(callback *tgbotapi.CallbackQuery) {
	chatID := callback.Message.Chat.ID
	data := callback.Data
	callbackID := callback.ID

	// Проверяем наличие сессии
	b.SessionMu.RLock()
	sess, ok := b.UserSessions[chatID]
	b.SessionMu.RUnlock()
	if !ok {
		b.BotAPI.Request(tgbotapi.NewCallback(callbackID, "Ошибка сессии. Начните заново."))
		return
	}

	b.Mu.Lock()
	defer b.Mu.Unlock()

	userSettings, exists := b.Users[chatID]
	if !exists {
		userSettings = &models.UserSettings{}
		b.Users[chatID] = userSettings
	}

	switch {
	// Кнопка "назад"
	case data == "back":
		b.popState(chatID)
		b.renderState(chatID)

	case data == "back_to_start":
		b.SessionMu.Lock()
		b.UserSessions[chatID].States = []string{"welcome"}
		b.SessionMu.Unlock()
		b.Users[chatID] = &models.UserSettings{}
		b.renderState(chatID)

	case data == "to:description":
		b.showDescription(chatID)

	case data == "to:disclaimer":
		b.showDisclaimer(chatID)

	case data == "to:choose_main_mode":
		b.pushState(chatID, "choose_main_mode")
		b.renderState(chatID)

	case strings.HasPrefix(data, "to:"):
		newSt := strings.TrimPrefix(data, "to:")
		b.pushState(chatID, newSt)
		b.renderState(chatID)

	// Price Change: set_change:
	case strings.HasPrefix(data, "set_change:"):
		v := strings.TrimPrefix(data, "set_change:")
		th, err := strconv.ParseFloat(v, 64)
		if err != nil {
			b.editError(chatID, sess)
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
			return
		}
		userSettings.ChangeThreshold = th

		if b.currentState(chatID) == "pumps_dumps" {
			b.pushState(chatID, "choose_timeframe")
		}
		b.renderState(chatID)

	case strings.HasPrefix(data, "set_time:"):
		tf := strings.TrimPrefix(data, "set_time:")
		userSettings.TimeFrame = tf
		b.pushState(chatID, "choose_target_bot")
		b.renderState(chatID)

	case strings.HasPrefix(data, "target:"):
		bn := strings.TrimPrefix(data, "target:")
		userSettings.TargetBot = bn
		// вызываем колбэк, если есть
		if b.OnSettingsFn != nil {
			b.OnSettingsFn(chatID, *userSettings)
		}
		b.pushState(chatID, "final")
		b.renderState(chatID)

	//-----------------------------------------------------
	// OI thresholds
	case strings.HasPrefix(data, "set_oi_threshold:"):
		v := strings.TrimPrefix(data, "set_oi_threshold:")
		th, err := strconv.ParseFloat(v, 64)
		if err != nil {
			b.editError(chatID, sess)
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
			return
		}
		userSettings.MonitorOI = true
		userSettings.OIThreshold = th
		b.pushState(chatID, "choose_oi_timeframe")
		b.renderState(chatID)

	case strings.HasPrefix(data, "set_oi_timeframe:"):
		tf := strings.TrimPrefix(data, "set_oi_timeframe:")
		userSettings.OITimeFrames = append(userSettings.OITimeFrames, tf)
		b.pushState(chatID, "choose_oi_threshold")
		b.renderState(chatID)

	case strings.HasPrefix(data, "set_oi_threshold_value:"):
		v := strings.TrimPrefix(data, "set_oi_threshold_value:")
		th, err := strconv.ParseFloat(v, 64)
		if err != nil {
			b.editError(chatID, sess)
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
			return
		}
		userSettings.OIThresholds = append(userSettings.OIThresholds, th)
		b.pushState(chatID, "choose_target_bot")
		b.renderState(chatID)

	//-----------------------------------------------------
	// Intraday pumps/dumps
	case strings.HasPrefix(data, "set_pd:"):
		parts := strings.Split(strings.TrimPrefix(data, "set_pd:"), ":")
		if len(parts) != 2 {
			b.editError(chatID, sess)
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
			return
		}
		pdPercent, err1 := strconv.ParseFloat(parts[0], 64)
		pdMinutes, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			b.editError(chatID, sess)
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
			return
		}
		userSettings.SomeIntradayThreshold = pdPercent
		userSettings.TimeFrame = fmt.Sprintf("%dm", pdMinutes)
		userSettings.SomeIntradayMetric = "pumps_dumps"
		b.pushState(chatID, "choose_target_bot")
		b.renderState(chatID)

	case strings.HasPrefix(data, "set_intraday_metric:"):
		v := strings.TrimPrefix(data, "set_intraday_metric:")
		userSettings.SomeIntradayMetric = v
		b.pushState(chatID, "choose_intraday_threshold")
		b.renderState(chatID)

	case strings.HasPrefix(data, "set_intraday_threshold:"):
		v := strings.TrimPrefix(data, "set_intraday_threshold:")
		threshold, err := strconv.ParseFloat(v, 64)
		if err != nil {
			b.editError(chatID, sess)
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
			return
		}
		userSettings.SomeIntradayThreshold = threshold
		b.pushState(chatID, "choose_target_bot")
		b.renderState(chatID)

	//-----------------------------------------------------
	// Spot volume
	case strings.HasPrefix(data, "set_spot_metric:"):
		v := strings.TrimPrefix(data, "set_spot_metric:")
		userSettings.SpotMetric = v
		b.pushState(chatID, "choose_spot_threshold")
		b.renderState(chatID)

	case strings.HasPrefix(data, "set_spot_threshold:"):
		v := strings.TrimPrefix(data, "set_spot_threshold:")
		th, err := strconv.ParseFloat(v, 64)
		if err != nil {
			b.editError(chatID, sess)
			b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
			return
		}
		userSettings.SpotVolumeMultiplier = th
		b.pushState(chatID, "choose_spot_interval")
		b.renderState(chatID)

	case strings.HasPrefix(data, "set_spot_interval:"):
		interval := strings.TrimPrefix(data, "set_spot_interval:")
		userSettings.SpotInterval = interval
		userSettings.MonitorSpot = true
		b.pushState(chatID, "choose_target_bot")
		b.renderState(chatID)

	//-----------------------------------------------------
	// Delta
	case strings.HasPrefix(data, "set_delta_interval:"):
		interval := strings.TrimPrefix(data, "set_delta_interval:")
		userSettings.MonitorDelta = true
		userSettings.DeltaInterval = interval
		if b.OnSettingsFn != nil {
			b.OnSettingsFn(chatID, *userSettings)
		}
		b.pushState(chatID, "choose_target_bot")
		b.renderState(chatID)
		b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
		return

	default:
		b.sendUnknown(chatID)
	}

	b.BotAPI.Request(tgbotapi.NewCallback(callbackID, ""))
}

// formatDeltaResults — пример форматтера для Delta
func (b *Bot) formatDeltaResults(results []binance.SymbolDelta, interval string) string {
	if len(results) == 0 {
		return fmt.Sprintf("Delta (%s): нет данных", interval)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🌊 *Delta Top-10* за период: `%s`\n\n", interval))

	sort.Slice(results, func(i, j int) bool {
		return results[i].Delta > results[j].Delta
	})

	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d) %s\n", i+1, r.Symbol))
		sb.WriteString(fmt.Sprintf("   BuyVol=%.2f\n   SellVol=%.2f\n   Delta=%.2f\n   Δ%%=%.2f%%\n\n",
			r.BuyVolume, r.SellVolume, r.Delta, r.RatioToDailyVolume))
	}
	return sb.String()
}

// pushState — добавить новое состояние
func (b *Bot) pushState(chatID int64, st string) {
	b.SessionMu.Lock()
	defer b.SessionMu.Unlock()

	sess := b.UserSessions[chatID]
	sess.States = append(sess.States, st)
}

// popState — вернуться на состояние назад
func (b *Bot) popState(chatID int64) {
	b.SessionMu.Lock()
	defer b.SessionMu.Unlock()

	sess := b.UserSessions[chatID]
	if len(sess.States) > 1 {
		sess.States = sess.States[:len(sess.States)-1]
	}
}

// currentState — текущее состояние
func (b *Bot) currentState(chatID int64) string {
	b.SessionMu.RLock()
	defer b.SessionMu.RUnlock()

	sess := b.UserSessions[chatID]
	return sess.States[len(sess.States)-1]
}

// renderState — отрисовываем текущее состояние
func (b *Bot) renderState(chatID int64) {
	b.SessionMu.RLock()
	sess, ok := b.UserSessions[chatID]
	b.SessionMu.RUnlock()
	if !ok {
		b.sendUnknown(chatID)
		return
	}

	state := b.currentState(chatID)
	var text string
	var btn tgbotapi.InlineKeyboardMarkup

	switch state {
	case "welcome":
		text = "🚀 Привет!\n\nСначала ознакомься с первыми двумя кнопками, там немного, но полезно, потом приступай к третьей\n\nВыберите действие:"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("📄 Функционал", "to:description"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⚠️ Отказ от ответственности", "to:disclaimer"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🚀 Поехали", "to:choose_main_mode"),
			),
		)

	case "description":
		text = "📄 *Описание*\n\nВыбираешь режим, в нём выбираешь метрику.\n\nТам - проценты и таймфреймы, после - целевой бот, в которого будут приходить метрики.\n\nВсего ботов 4, при первом запуске прокликай их все, чтобы активировать их и чтобы они могли присылать уведомления.\n\nОтдельно выбранная метрика - отдельный бот. Можешь выбрать метрику для любого из ботов, можешь всё в одного, на твое усмотрение\n\nУдачи ✌🏻"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "disclaimer":
		text = "⚠️ *Отказ от ответственности*\n\nДанные и функционал этого бота предоставляются исключительно в информационных целях.\n\nМы не несем ответственности за любые инвестиционные решения, принятые на основании этих данных, а также за любые убытки или упущенную выгоду, возникшие в результате их использования.\n\nВы самостоятельно принимаете решения и несете ответственность за все последствия. Мы лишь предоставляем инструмент для анализа, но не влияем на ваши действия и не несем ответственности за ваши убытки или прибыль.\n\nВсем мира, добра и зеленых PNL✌🏻"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "choose_main_mode":
		text = "📂 На выбор 3 режима. Данные берутся с Binance API и веб-сокетов в реальном времени\n\nВ каждом разделе - описание функционала\n\nВыберите режим:"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⚡ Scalp Mode", "to:scalp_mode"),
				tgbotapi.NewInlineKeyboardButtonData("👁 Intraday", "to:intraday_mode"),
				tgbotapi.NewInlineKeyboardButtonData("💎 Spot Mode", "to:spot_mode"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	// SCALP MODE
	case "scalp_mode":
		b.Users[chatID].Mode = "scalp"
		text = "💨 Scalp Mode\n\nПолучайте быстрые уведомления о изменениях цен на Binance, широкий выбор процентов изменения и таймфреймов\n\nФункционал будет дополняться, мы вас оповестим 🗣"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🎰 Pumps/Dumps", "to:pumps_dumps"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "pumps_dumps":
		text = "🎰 Отслеживание резких краткосрочных скачков и падений цен. Эта метрика помогает выявить стремительные изменения рынка в режиме скальпинга, позволяя оперативно реагировать на происходящее на бирже\n\nПорог изменения цены (%):"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ 2%", "set_change:2"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 2.1%", "set_change:2.1"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 2.5%", "set_change:2.5"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ 3%", "set_change:3"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 5%", "set_change:5"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ 7%", "set_change:7"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 10%", "set_change:10"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "choose_timeframe":
		text = "⏱ Выберите интервал (TimeFrame) для Scalp:"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("1m", "set_time:1m"),
				tgbotapi.NewInlineKeyboardButtonData("3m", "set_time:3m"),
				tgbotapi.NewInlineKeyboardButtonData("5m", "set_time:5m"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("15m", "set_time:15m"),
				tgbotapi.NewInlineKeyboardButtonData("30m", "set_time:30m"),
				tgbotapi.NewInlineKeyboardButtonData("1h", "set_time:1h"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	// INTRADAY
	case "intraday_mode":
		b.Users[chatID].Mode = "intraday"
		text = "⏱ Intraday Mode\n\nПолучайте оповещения о необычной торговой активности, изменении открытого интереса и отслеживайте разницу между объемами покупок и продаж на биржах\n\nВыберите одну или несколько метрик, дальше выберите таймфрейм и процент изменения, после - целевого бота, в которого будут приходить выбранные метрики\n\nФункционал будет дополняться, мы вас оповестим 🗣"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("📊 Изменение OI", "to:choose_oi_timeframe"),
				tgbotapi.NewInlineKeyboardButtonData("🎰 Pumps/Dumps", "to:intraday_pumps_dumps"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🌊 Delta", "to:intraday_delta"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "choose_oi_timeframe":
		text = "⏱ Показывает динамику изменения открытого интереса в течение торгового дня и/или по выбранному таймфрейму. Анализирует, как изменяется количество открытых позиций по активам, что может указывать на спрос или растущую активность в определённый момент\n\n Выберите таймфрейм для OI:"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("5m", "set_oi_timeframe:5m"),
				tgbotapi.NewInlineKeyboardButtonData("15m", "set_oi_timeframe:15m"),
				tgbotapi.NewInlineKeyboardButtonData("30m", "set_oi_timeframe:30m"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("1h", "set_oi_timeframe:1h"),
				tgbotapi.NewInlineKeyboardButtonData("4h", "set_oi_timeframe:4h"),
				tgbotapi.NewInlineKeyboardButtonData("1d", "set_oi_timeframe:1d"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "choose_oi_threshold":
		text = "📊 Выберите порог изменения OI (%):"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ 2.5%", "set_oi_threshold_value:2.5"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 5%", "set_oi_threshold_value:5"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ 10%", "set_oi_threshold_value:10"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 15%", "set_oi_threshold_value:15"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "intraday_pumps_dumps":
		text = "📈 Фокусируется на выявлении и анализе значительных ценовых выбросов и падений. На выбор - 5 режимов\n\n Выберите параметр Pumps/Dumps (процент / TF):"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("3% / 5m", "set_pd:3:5"),
				tgbotapi.NewInlineKeyboardButtonData("5% / 15m", "set_pd:5:15"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("7% / 30m", "set_pd:7:30"),
				tgbotapi.NewInlineKeyboardButtonData("10% / 60m", "set_pd:10:60"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("15% / 240m", "set_pd:15:240"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "intraday_delta":
		text = "🌊 Показывает разницу между объёмами покупок и продаж за определённый период. Эта метрика помогает понять баланс сил между покупателями и продавцами, что может служить сигналом к возможному продолжению тренда или развороту\n\nВыберите период для Delta анализа:"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("15m", "set_delta_interval:15m"),
				tgbotapi.NewInlineKeyboardButtonData("1h", "set_delta_interval:1h"),
				tgbotapi.NewInlineKeyboardButtonData("24h", "set_delta_interval:24h"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "spot_mode":
		b.Users[chatID].Mode = "spot"
		text = "💰 Spot Mode\n\nПолучайте уведомления о аномальной активности объемов на Binance Spot\n\nНа выбор несколько порогов увеличения объема торгов и 4 таймфрейма на выбор для комфортной торговли\n\nФункционал будет дополняться, мы вас оповестим 🗣"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔊 Объём торгов", "set_spot_metric:volume"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "choose_spot_threshold":
		text = "📊 Отслеживает общий объём торгов на спотовом рынке. Высокие значения объёма могут свидетельствовать о значительной рыночной активности, появлении новых участников или крупных сделках, влияющих на ликвидность и волатильность\n\nВыберите порог увеличения объёма (x):"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ 2x", "set_spot_threshold:2"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 3x", "set_spot_threshold:3"),
				tgbotapi.NewInlineKeyboardButtonData("✅ 4x", "set_spot_threshold:4"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "choose_spot_interval":
		text = "⏱ Выберите интервал для объёма (Spot):"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("1h", "set_spot_interval:1h"),
				tgbotapi.NewInlineKeyboardButtonData("4h", "set_spot_interval:4h"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("12h", "set_spot_interval:12h"),
				tgbotapi.NewInlineKeyboardButtonData("24h", "set_spot_interval:24h"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "choose_target_bot":
		text = "🤖 Выберите бота для уведомлений:"
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🟥 Bot1", "target:bot1"),
				tgbotapi.NewInlineKeyboardButtonData("🟩 Bot2", "target:bot2"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🟨 Bot3", "target:bot3"),
				tgbotapi.NewInlineKeyboardButtonData("🟦 Bot4", "target:bot4"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
			),
		)

	case "final":
		s := b.Users[chatID]
		botUsername, ok := b.ManagerRef.Usernames[s.TargetBot]
		if !ok {
			botUsername = "unknown_bot"
		}
		link := fmt.Sprintf("https://t.me/%s", botUsername)
		text = fmt.Sprintf("✅ Мониторинг запущен!\nНажмите кнопку: [t.me/%s](%s)", botUsername, link)
		btn = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonURL("👉 Перейти в бота", link),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "back"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("◀️ Вернуться в начало", "to:choose_main_mode"),
			),
		)

	default:
		text = "❓ Неизвестное состояние"
		btn = tgbotapi.NewInlineKeyboardMarkup()
	}

	edit := tgbotapi.NewEditMessageTextAndMarkup(sess.ChatID, sess.MessageID, text, btn)
	edit.ParseMode = "Markdown"

	if _, err := b.BotAPI.Send(edit); err != nil {
		log.Printf("Error editing message for chat %d: %v", chatID, err)
	}
}

// startCommand — /start
func (b *Bot) startCommand(chatID int64, firstName string) {
	msgText := fmt.Sprintf("🚀 Привет, %s! Добро пожаловать.\n\nСначала ознакомься с первыми двумя кнопками, там немного, но полезно, потом приступай к третьей\n\n", firstName)
	btn := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📄 Описание функционала", "to:description"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚠️ Отказ от ответственности", "to:disclaimer"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🚀 Поехали", "to:choose_main_mode"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ReplyMarkup = btn

	sentMsg, err := b.BotAPI.Send(msg)
	if err != nil {
		log.Printf("Error sending start message to user %d: %v", chatID, err)
		return
	}

	b.SessionMu.Lock()
	b.UserSessions[chatID] = &userSession{
		ChatID:    chatID,
		MessageID: sentMsg.MessageID,
		States:    []string{"welcome"},
	}
	b.SessionMu.Unlock()
}

// sendHelp — /help
func (b *Bot) sendHelp(chatID int64) {
	helpText := "📖 *Справка по боту:*\n\n" +
		"/start - Начать\n" +
		"/help - Справка\n\n" +
		"Этот бот отслеживает изменения цены, объёма, OI и т.д."
	msg := tgbotapi.NewMessage(chatID, helpText)
	msg.ParseMode = "Markdown"
	b.BotAPI.Send(msg)
}

// sendUnknown — неизвестная команда
func (b *Bot) sendUnknown(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "❓ Неизвестная команда. Используйте /start или /help.")
	b.BotAPI.Send(msg)
}

// showDescription — кнопка "функционал"
func (b *Bot) showDescription(chatID int64) {
	text := "💡 *Описание*\n\nВыбираешь режим, в нём выбираешь метрику.\n\nТам - проценты и таймфреймы, после - целевой бот, в которого будут приходить метрики.\n\nВсего ботов 4, при первом запуске прокликай их все, чтобы активировать их и чтобы они могли присылать уведомления.\n\nОтдельно выбранная метрика - отдельный бот. Можешь выбрать метрику для любого из ботов, можешь всё в одного, на твое усмотрение\n\nУдачи ✌🏻"
	btn := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
		),
	)

	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, b.UserSessions[chatID].MessageID, text, btn)
	edit.ParseMode = "Markdown"
	b.BotAPI.Send(edit)
}

// showDisclaimer — кнопка "дисклеймер"
func (b *Bot) showDisclaimer(chatID int64) {
	text := "⚠️*Отказ от ответственности*\n\nДанные и функционал этого бота предоставляются исключительно в информационных целях.\n\nМы не несем ответственности за любые инвестиционные решения, принятые на основании этих данных, а также за любые убытки или упущенную выгоду, возникшие в результате их использования.\n\nВы самостоятельно принимаете торговые решения и несете ответственность за все последствия. Мы лишь предоставляем инструмент для анализа, но не влияем на ваши действия и не несем ответственности за ваш торговый баланс.\n\nВсем мира, добра и зеленых PNL✌🏻"
	btn := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
		),
	)
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, b.UserSessions[chatID].MessageID, text, btn)
	edit.ParseMode = "Markdown"
	b.BotAPI.Send(edit)
}

// editError — показываем сообщение об ошибке
func (b *Bot) editError(chatID int64, sess *userSession) {
	text := "❌ Произошла ошибка. Попробуйте снова."
	btn := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back"),
		),
	)
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, sess.MessageID, text, btn)
	edit.ParseMode = "Markdown"
	b.BotAPI.Send(edit)
}
