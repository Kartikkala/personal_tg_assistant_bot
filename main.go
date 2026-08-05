package main

import (
	"fmt"
	"log"
	"os"

	"ai_enforcer/bot"
	"ai_enforcer/db"
	"ai_enforcer/llm"
	"ai_enforcer/orchestrator"
)

func main() {
	dbConnStr := os.Getenv("DATABASE_URL")
	if dbConnStr == "" {
		log.Fatal("DATABASE_URL environment variable is required (e.g. postgres://user:pass@localhost:5432/dbname?sslmode=disable)")
	}

	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if telegramToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	if geminiAPIKey == "" {
		log.Fatal("GEMINI_API_KEY environment variable is required")
	}

	database, err := db.NewPostgresDB(dbConnStr)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	telegramChatIDStr := os.Getenv("TELEGRAM_CHAT_ID")
	var telegramChatID int64
	if telegramChatIDStr != "" {
		fmt.Sscanf(telegramChatIDStr, "%d", &telegramChatID)
	}

	telegramBot, err := bot.NewTelegramBot(telegramToken, telegramChatID)
	if err != nil {
		log.Fatalf("Failed to initialize Telegram bot: %v", err)
	}

	geminiClient, err := llm.NewGeminiClient(geminiAPIKey)
	if err != nil {
		log.Fatalf("Failed to initialize Gemini client: %v", err)
	}

	callMeBotUser := os.Getenv("CALLMEBOT_USER")
	if callMeBotUser == "" {
		log.Println("CALLMEBOT_USER environment variable is not set. Phone calls will be disabled.")
	}

	orch := orchestrator.NewOrchestrator(database, geminiClient, telegramBot, callMeBotUser)

	messageChan := make(chan string)

	log.Println("Starting Telegram listener...")
	go telegramBot.StartListening(messageChan)

	log.Println("Starting Orchestrator...")
	orch.Start(messageChan)
}
