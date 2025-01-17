package bots

import (
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	log "github.com/sirupsen/logrus"
)

// BotManager управляет всеми ботами
type BotManager struct {
	Bots      map[string]*Bot
	Usernames map[string]string
	mu        sync.RWMutex
}

// NewBotManager создаёт новый BotManager
func NewBotManager(mainToken string, additional map[string]string, usernames map[string]string) (*BotManager, error) {
	manager := &BotManager{
		Bots:      make(map[string]*Bot),
		Usernames: usernames,
	}

	mainBot, err := NewBot(mainToken)
	if err != nil {
		return nil, err
	}
	manager.mu.Lock()
	manager.Bots["main"] = mainBot
	manager.mu.Unlock()
	mainBot.ManagerRef = manager

	// Добавляем дополнительных ботов
	for name, token := range additional {
		bot, err := NewBot(token)
		if err != nil {
			log.Errorf("Ошибка инициализации дополнительного бота %s: %v", name, err)
			continue
		}
		manager.mu.Lock()
		manager.Bots[name] = bot
		manager.mu.Unlock()
		bot.ManagerRef = manager
	}

	manager.Usernames = usernames
	return manager, nil
}

// SendToBot отправляет сообщение через указанный бот
func (m *BotManager) SendToBot(botName string, chatID int64, text string) {
	m.mu.RLock()
	b, ok := m.Bots[botName]
	m.mu.RUnlock()
	if !ok {
		log.Warnf("Бот с именем %s не найден. Сообщение не отправлено.", botName)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	_, err := b.BotAPI.Send(msg)
	if err != nil {
		log.Errorf("Ошибка при отправке сообщения через бота %s: %v", botName, err)
	} else {
		log.Infof("Отправлено сообщение пользователю %d через бота %s: %s", chatID, botName, text)
	}
}
