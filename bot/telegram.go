package bot

import (
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type TelegramBot struct {
	api    *tgbotapi.BotAPI
	chatID int64 // The user's chat ID
}

func NewTelegramBot(token string, chatID int64) (*TelegramBot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	return &TelegramBot{
		api:    api,
		chatID: chatID,
	}, nil
}

// StartListening listens for messages and sends them to the returned channel.
// It latches onto the first chat ID it receives a message from.
func (b *TelegramBot) StartListening(messageChan chan<- string) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		if update.Message != nil {
			if b.chatID == 0 {
				b.chatID = update.Message.Chat.ID
				log.Printf("Locked onto chat ID: %d", b.chatID)
			}
			
			if update.Message.Chat.ID == b.chatID {
				messageChan <- update.Message.Text
			}
		}
	}
}

func (b *TelegramBot) SendMessage(text string) error {
	if b.chatID == 0 {
		return nil // Nowhere to send yet
	}
	msg := tgbotapi.NewMessage(b.chatID, text)
	_, err := b.api.Send(msg)
	return err
}

func (b *TelegramBot) SetChatID(id int64) {
	b.chatID = id
}
