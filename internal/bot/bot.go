package bot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/flash4612/dnd-telegram-bot/internal/config"
	"github.com/flash4612/dnd-telegram-bot/internal/format"
	"github.com/flash4612/dnd-telegram-bot/internal/store"
	"github.com/flash4612/dnd-telegram-bot/internal/voice"
)

type Bot struct {
	api      *tgbotapi.BotAPI
	cfg      config.Config
	store    *store.Store
	voice    *voice.Transcriber
	mu       sync.Mutex
	pending  map[int64][]store.SearchHit
}

func New(api *tgbotapi.BotAPI, cfg config.Config, st *store.Store, tr *voice.Transcriber) *Bot {
	return &Bot{
		api:     api,
		cfg:     cfg,
		store:   st,
		voice:   tr,
		pending: make(map[int64][]store.SearchHit),
	}
}

func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return ctx.Err()
		case upd, ok := <-updates:
			if !ok {
				return nil
			}
			if err := b.handleUpdate(ctx, upd); err != nil {
				log.Printf("handle update: %v", err)
			}
		}
	}
}

func (b *Bot) handleUpdate(ctx context.Context, upd tgbotapi.Update) error {
	msg := upd.Message
	if msg == nil {
		return nil
	}
	if msg.From == nil || !b.cfg.IsAllowed(msg.From.ID) {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "Доступ закрыт. Добавь свой Telegram user id в ALLOWED_USER_IDS.")
		_, _ = b.api.Send(reply)
		return nil
	}

	switch {
	case msg.IsCommand():
		return b.handleCommand(ctx, msg)
	case msg.Voice != nil:
		return b.handleVoice(ctx, msg)
	case strings.TrimSpace(msg.Text) != "":
		return b.handleQuery(ctx, msg, strings.TrimSpace(msg.Text))
	default:
		return nil
	}
}

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message) error {
	switch msg.Command() {
	case "start", "help":
		return b.sendText(msg.Chat.ID, format.HelpMessage())
	case "stats":
		counts, err := b.store.CountByCategory(ctx)
		if err != nil {
			return err
		}
		total, err := b.store.Count(ctx)
		if err != nil {
			return err
		}
		return b.sendText(msg.Chat.ID, format.StatsMessage(counts, total))
	default:
		return b.sendText(msg.Chat.ID, "Неизвестная команда. /help")
	}
}

func (b *Bot) handleVoice(ctx context.Context, msg *tgbotapi.Message) error {
	if b.voice == nil || !b.voice.Enabled() {
		return b.sendText(msg.Chat.ID, "Голосовые пока выключены: добавь OPENAI_API_KEY в .env и перезапусти бота.")
	}

	_, _ = b.api.Send(tgbotapi.NewChatAction(msg.Chat.ID, tgbotapi.ChatTyping))

	fileURL, err := b.api.GetFileDirectURL(msg.Voice.FileID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download voice: status %d", resp.StatusCode)
	}

	text, err := b.voice.Transcribe(ctx, "voice.ogg", resp.Body)
	if err != nil {
		return b.sendText(msg.Chat.ID, "Не смог разобрать голос: "+err.Error())
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return b.sendText(msg.Chat.ID, "Пустая расшифровка. Попробуй ещё раз чётче.")
	}
	_ = b.sendText(msg.Chat.ID, "Распознал: "+text)
	return b.handleQuery(ctx, msg, text)
}

func (b *Bot) handleQuery(ctx context.Context, msg *tgbotapi.Message, query string) error {
	if n, err := strconv.Atoi(query); err == nil {
		if hit, ok := b.pickPending(msg.From.ID, n); ok {
			return b.sendEntry(msg.Chat.ID, hit.Entry)
		}
	}

	_, _ = b.api.Send(tgbotapi.NewChatAction(msg.Chat.ID, tgbotapi.ChatTyping))

	hits, err := b.store.Search(ctx, query, 8)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return b.sendText(msg.Chat.ID, "Ничего не нашёл по запросу «"+query+"».")
	}

	best := hits[0]
	exact := equalFold(best.TitleRU, query) || equalFold(best.TitleEN, query)
	if exact || len(hits) == 1 || clearlyBest(hits) {
		b.clearPending(msg.From.ID)
		return b.sendEntry(msg.Chat.ID, best.Entry)
	}

	b.setPending(msg.From.ID, hits)
	return b.sendText(msg.Chat.ID, format.ChoicesMessage(hits))
}

func clearlyBest(hits []store.SearchHit) bool {
	if len(hits) < 2 {
		return true
	}
	a := normalize(hits[0].TitleRU + " " + hits[0].TitleEN)
	b := normalize(hits[1].TitleRU + " " + hits[1].TitleEN)
	return strings.Contains(a, normalize(hits[0].TitleRU)) && !strings.Contains(b, normalize(hits[0].TitleRU))
}

func (b *Bot) sendEntry(chatID int64, e store.Entry) error {
	for _, part := range format.EntryMessages(e) {
		if err := b.sendText(chatID, part); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) sendText(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = true
	_, err := b.api.Send(msg)
	return err
}

func (b *Bot) setPending(userID int64, hits []store.SearchHit) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[userID] = hits
}

func (b *Bot) clearPending(userID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, userID)
}

func (b *Bot) pickPending(userID int64, n int) (store.SearchHit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hits := b.pending[userID]
	if n < 1 || n > len(hits) {
		return store.SearchHit{}, false
	}
	hit := hits[n-1]
	delete(b.pending, userID)
	return hit, true
}

func equalFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}
