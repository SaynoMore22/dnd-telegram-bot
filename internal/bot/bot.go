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

	"github.com/SaynoMore22/dnd-telegram-bot/internal/config"
	"github.com/SaynoMore22/dnd-telegram-bot/internal/format"
	"github.com/SaynoMore22/dnd-telegram-bot/internal/store"
	"github.com/SaynoMore22/dnd-telegram-bot/internal/voice"
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
	if err := b.setupMenuButton(); err != nil {
		log.Printf("setup menu button: %v", err)
	}

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

// setupMenuButton регистрирует список команд и включает кнопку "Menu" рядом с полем
// ввода в Telegram-клиенте (по умолчанию она вообще не появляется, пока боту не
// передан непустой список команд через setMyCommands).
func (b *Bot) setupMenuButton() error {
	cmds := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "menu", Description: "Меню разделов"},
		tgbotapi.BotCommand{Command: "start", Description: "Начало работы"},
		tgbotapi.BotCommand{Command: "help", Description: "Справка"},
		tgbotapi.BotCommand{Command: "stats", Description: "Сколько записей в базе"},
	)
	if _, err := b.api.Request(cmds); err != nil {
		return fmt.Errorf("set my commands: %w", err)
	}

	params := tgbotapi.Params{}
	if err := params.AddInterface("menu_button", map[string]string{"type": "commands"}); err != nil {
		return err
	}
	if _, err := b.api.MakeRequest("setChatMenuButton", params); err != nil {
		return fmt.Errorf("set chat menu button: %w", err)
	}
	return nil
}

func (b *Bot) handleUpdate(ctx context.Context, upd tgbotapi.Update) error {
	if cq := upd.CallbackQuery; cq != nil {
		if cq.From == nil || !b.cfg.IsAllowed(cq.From.ID) || cq.Message == nil {
			return nil
		}
		return b.handleCallback(ctx, cq)
	}

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
	case "start", "menu":
		return b.sendMessages(msg.Chat.ID, []format.Message{format.MainMenuMessage()})
	case "help":
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
			return b.sendEntry(ctx, msg.Chat.ID, hit.Entry)
		}
	}

	_, _ = b.api.Send(tgbotapi.NewChatAction(msg.Chat.ID, tgbotapi.ChatTyping))

	hits, err := b.store.Search(ctx, query, 8)
	if err != nil {
		return err
	}
	only2014 := false
	if len(hits) == 0 {
		hits2014, err := b.store.SearchEdition(ctx, query, store.Edition2014, 8)
		if err != nil {
			return err
		}
		if len(hits2014) == 0 {
			return b.sendText(msg.Chat.ID, "Ничего не нашёл по названию «"+query+"». Попробуй точнее или другое написание (RU/EN).")
		}
		hits = hits2014
		only2014 = true
	}

	best := hits[0]
	if isStrongTitleMatch(query, best) || best.Score <= 1 {
		if len(hits) == 1 || clearlyBetterTitle(hits) || isStrongTitleMatch(query, best) {
			b.clearPending(msg.From.ID)
			if only2014 {
				_ = b.sendText(msg.Chat.ID, "ℹ️ В редакции 2024 не нашёл — показываю версию 2014.")
			}
			return b.sendEntry(ctx, msg.Chat.ID, best.Entry)
		}
	}

	b.setPending(msg.From.ID, hits)
	return b.sendText(msg.Chat.ID, format.ChoicesMessage(hits))
}

func isStrongTitleMatch(query string, hit store.SearchHit) bool {
	q := normalize(query)
	for _, p := range []string{"заклинание ", "спелл ", "spell ", "предмет ", "класс ", "вид ", "предыстория ", "снаряжение "} {
		if strings.HasPrefix(q, p) {
			q = strings.TrimSpace(strings.TrimPrefix(q, p))
		}
	}
	return equalFold(hit.TitleRU, q) || equalFold(hit.TitleEN, q) ||
		strings.HasPrefix(normalize(hit.TitleRU), q) ||
		strings.HasPrefix(normalize(hit.TitleEN), q)
}

func clearlyBetterTitle(hits []store.SearchHit) bool {
	if len(hits) < 2 {
		return true
	}
	return hits[0].Score < hits[1].Score
}

func (b *Bot) sendEntry(ctx context.Context, chatID int64, e store.Entry) error {
	var msgs []format.Message
	if e.Category == store.CategoryClass {
		if cd, ok := format.ParseClassData(e.Body); ok {
			msgs = []format.Message{format.ClassOverviewMessage(e, cd)}
		}
	}
	if msgs == nil {
		if ad, ok := format.ParseArticleData(e.Body); ok {
			msgs = []format.Message{format.ArticleOverviewMessage(e, ad)}
		}
	}
	if msgs == nil {
		msgs = format.EntryMessages(e)
	}
	if len(msgs) == 0 {
		return nil
	}

	otherEdition := store.Edition2014
	if e.Edition == store.Edition2014 {
		otherEdition = store.Edition2024
	}
	if counterpart, ok, err := b.store.FindByTitle(ctx, e.Category, otherEdition, e.TitleRU, e.TitleEN); err == nil && ok {
		last := len(msgs) - 1
		btn := format.EditionSwitchButton(counterpart.ID, otherEdition)
		msgs[last].Buttons = append(msgs[last].Buttons, []format.Button{btn})
	}

	return b.sendMessages(chatID, msgs)
}

func (b *Bot) sendMessages(chatID int64, msgs []format.Message) error {
	for _, part := range msgs {
		msg := tgbotapi.NewMessage(chatID, part.Text)
		msg.DisableWebPagePreview = true
		if part.ParseHTML {
			msg.ParseMode = "HTML"
		}
		if len(part.Buttons) > 0 {
			msg.ReplyMarkup = buildKeyboard(part.Buttons)
		}
		if _, err := b.api.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

// handleMenuCallback обрабатывает навигацию по кнопочному меню /start:
// mn:home | mn:cat:<category> | mn:page:<category>:<letter|*>:<page> | mn:entry:<id>
func (b *Bot) handleMenuCallback(ctx context.Context, cq *tgbotapi.CallbackQuery, parts []string) error {
	chatID := cq.Message.Chat.ID
	if len(parts) == 0 {
		return nil
	}

	switch parts[0] {
	case "home":
		return b.editOrSend(cq, format.MainMenuMessage())

	case "cat":
		if len(parts) < 2 {
			return nil
		}
		category := store.Category(parts[1])
		items, err := b.store.ListTitles(ctx, category, store.Edition2024)
		if err != nil {
			return err
		}
		if format.NeedsLetterIndex(len(items)) {
			return b.editOrSend(cq, format.CategoryLettersMessage(category, items))
		}
		return b.editOrSend(cq, format.CategoryPageMessage(category, "*", items, 0))

	case "page":
		if len(parts) < 4 {
			return nil
		}
		category := store.Category(parts[1])
		letter := parts[2]
		page, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil
		}
		items, err := b.store.ListTitles(ctx, category, store.Edition2024)
		if err != nil {
			return err
		}
		return b.editOrSend(cq, format.CategoryPageMessage(category, letter, items, page))

	case "entry":
		if len(parts) < 2 {
			return nil
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil
		}
		entry, err := b.store.GetByID(ctx, id)
		if err != nil {
			return err
		}
		return b.sendEntry(ctx, chatID, entry)
	}
	return nil
}

// editOrSend правит текущее сообщение меню на месте (чтобы листание разделов не заваливало
// чат новыми сообщениями); если редактирование невозможно (например, сообщение слишком
// старое), отправляет как новое.
func (b *Bot) editOrSend(cq *tgbotapi.CallbackQuery, m format.Message) error {
	chatID := cq.Message.Chat.ID
	edit := tgbotapi.NewEditMessageText(chatID, cq.Message.MessageID, m.Text)
	if m.ParseHTML {
		edit.ParseMode = "HTML"
	}
	if len(m.Buttons) > 0 {
		kb := buildKeyboard(m.Buttons)
		edit.ReplyMarkup = &kb
	}
	if _, err := b.api.Send(edit); err != nil {
		return b.sendMessages(chatID, []format.Message{m})
	}
	return nil
}

func buildKeyboard(rows [][]format.Button) tgbotapi.InlineKeyboardMarkup {
	kb := make([][]tgbotapi.InlineKeyboardButton, 0, len(rows))
	for _, row := range rows {
		r := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
		for _, btn := range row {
			r = append(r, tgbotapi.NewInlineKeyboardButtonData(btn.Text, btn.Data))
		}
		kb = append(kb, r)
	}
	return tgbotapi.NewInlineKeyboardMarkup(kb...)
}

// handleCallback обрабатывает нажатия на inline-кнопки карточки класса
// (cf:<entryID>:table|feat|sublist|sub:<idx>).
func (b *Bot) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) error {
	ack := tgbotapi.NewCallback(cq.ID, "")
	defer func() { _, _ = b.api.Request(ack) }()

	parts := strings.Split(cq.Data, ":")
	if len(parts) < 2 {
		return nil
	}
	chatID := cq.Message.Chat.ID

	if parts[0] == "mn" {
		return b.handleMenuCallback(ctx, cq, parts[1:])
	}

	if parts[0] == "ed" {
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil
		}
		entry, err := b.store.GetByID(ctx, id)
		if err != nil {
			return err
		}
		return b.sendEntry(ctx, chatID, entry)
	}

	if parts[0] == "af" {
		if len(parts) < 3 {
			return nil
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil
		}
		idx, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil
		}
		entry, err := b.store.GetByID(ctx, id)
		if err != nil {
			return err
		}
		ad, ok := format.ParseArticleData(entry.Body)
		if !ok {
			return nil
		}
		return b.sendMessages(chatID, format.ArticleSectionMessages(ad, idx))
	}

	if len(parts) < 3 || parts[0] != "cf" {
		return nil
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil
	}
	entry, err := b.store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	cd, ok := format.ParseClassData(entry.Body)
	if !ok {
		return nil
	}

	switch parts[2] {
	case "table":
		return b.sendMessages(chatID, []format.Message{format.ClassTableMessage(cd)})
	case "feat":
		return b.sendMessages(chatID, format.ClassFeaturesMessages(cd))
	case "sublist":
		return b.sendMessages(chatID, []format.Message{format.ClassSubclassListMessage(entry, cd)})
	case "sub":
		if len(parts) < 4 {
			return nil
		}
		idx, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil
		}
		return b.sendMessages(chatID, format.ClassSubclassMessages(cd, idx))
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
