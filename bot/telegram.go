package bot

import (
	"fmt"
	"log"
	"strings"

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

	commands := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{
			Command:     "goal",
			Description: "Generate a detailed roadmap and schedule for a major goal",
		},
		tgbotapi.BotCommand{
			Command:     "tasks",
			Description: "Show all active tasks and goals",
		},
	)
	if _, err := api.Request(commands); err != nil {
		log.Printf("Failed to set bot commands: %v", err)
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

		if update.CallbackQuery != nil {
			if b.chatID == 0 {
				b.chatID = update.CallbackQuery.Message.Chat.ID
			}
			
			if update.CallbackQuery.Message.Chat.ID == b.chatID {
				// Acknowledge callback immediately to remove loading state
				callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "")
				if _, err := b.api.Request(callback); err != nil {
					log.Printf("Failed to answer callback: %v", err)
				}
				
				messageChan <- "[CALLBACK] " + update.CallbackQuery.Data
			}
		}
	}
}

func (b *TelegramBot) sendChunked(text string, isHTML bool) error {
	if b.chatID == 0 {
		return nil
	}

	const maxLen = 4000
	lines := strings.Split(text, "\n")
	var chunk strings.Builder

	for _, line := range lines {
		if chunk.Len()+len(line)+1 > maxLen {
			if chunk.Len() > 0 {
				msg := tgbotapi.NewMessage(b.chatID, chunk.String())
				if isHTML {
					msg.ParseMode = tgbotapi.ModeHTML
				}
				if _, err := b.api.Send(msg); err != nil {
					log.Printf("Error sending chunk: %v", err)
					return err
				}
				chunk.Reset()
			}
			
			for len(line) > maxLen {
				msg := tgbotapi.NewMessage(b.chatID, line[:maxLen])
				if isHTML {
					msg.ParseMode = tgbotapi.ModeHTML
				}
				if _, err := b.api.Send(msg); err != nil {
					log.Printf("Error sending giant line chunk: %v", err)
					return err
				}
				line = line[maxLen:]
			}
		}
		
		if chunk.Len() > 0 {
			chunk.WriteString("\n")
		}
		chunk.WriteString(line)
	}

	if chunk.Len() > 0 {
		msg := tgbotapi.NewMessage(b.chatID, chunk.String())
		if isHTML {
			msg.ParseMode = tgbotapi.ModeHTML
		}
		if _, err := b.api.Send(msg); err != nil {
			log.Printf("Error sending final chunk: %v", err)
			return err
		}
	}

	return nil
}

func (b *TelegramBot) SendMessage(text string) error {
	return b.sendChunked(text, false)
}

func (b *TelegramBot) SendMessageHTML(text string) error {
	return b.sendChunked(text, true)
}

func (b *TelegramBot) SendTypingAction() error {
	if b.chatID == 0 {
		return nil
	}
	action := tgbotapi.NewChatAction(b.chatID, tgbotapi.ChatTyping)
	_, err := b.api.Request(action)
	return err
}

func (b *TelegramBot) SetChatID(id int64) {
	b.chatID = id
}

func (b *TelegramBot) SendQuizQuestion(text string, options []string) (int, error) {
	if b.chatID == 0 {
		return 0, nil
	}

	var keyboard [][]tgbotapi.InlineKeyboardButton
	for i, opt := range options {
		data := fmt.Sprintf("quiz_answer_%d", i)
		btn := tgbotapi.NewInlineKeyboardButtonData(opt, data)
		keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(btn))
	}
	skipBtn := tgbotapi.NewInlineKeyboardButtonData("Skip Quiz", "quiz_skip")
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(skipBtn))

	msg := tgbotapi.NewMessage(b.chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(keyboard...)
	
	sentMsg, err := b.api.Send(msg)
	if err != nil {
		return 0, err
	}
	return sentMsg.MessageID, nil
}

func (b *TelegramBot) EditQuizQuestion(messageID int, text string, options []string) error {
	if b.chatID == 0 {
		return nil
	}

	var keyboard [][]tgbotapi.InlineKeyboardButton
	for i, opt := range options {
		data := fmt.Sprintf("quiz_answer_%d", i)
		btn := tgbotapi.NewInlineKeyboardButtonData(opt, data)
		keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(btn))
	}
	skipBtn := tgbotapi.NewInlineKeyboardButtonData("Skip Quiz", "quiz_skip")
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(skipBtn))

	msg := tgbotapi.NewEditMessageText(b.chatID, messageID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	markup := tgbotapi.NewInlineKeyboardMarkup(keyboard...)
	msg.ReplyMarkup = &markup
	
	_, err := b.api.Send(msg)
	return err
}

func (b *TelegramBot) EditQuizResult(messageID int, text string) error {
	if b.chatID == 0 {
		return nil
	}
	msg := tgbotapi.NewEditMessageText(b.chatID, messageID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	_, err := b.api.Send(msg)
	return err
}
