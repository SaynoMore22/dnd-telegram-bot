package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/SaynoMore22/dnd-telegram-bot/internal/bot"
	"github.com/SaynoMore22/dnd-telegram-bot/internal/config"
	"github.com/SaynoMore22/dnd-telegram-bot/internal/store"
	"github.com/SaynoMore22/dnd-telegram-bot/internal/voice"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer st.Close()

	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		log.Fatalf("telegram: %v", err)
	}
	log.Printf("authorized as @%s", api.Self.UserName)

	tr := voice.New(cfg.OpenAIKey)
	app := bot.New(api, cfg, st, tr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("bot stopped: %v", err)
	}
}
