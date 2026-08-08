package format

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/flash4612/dnd-telegram-bot/internal/store"
)

const telegramLimit = 4000

func EntryMessages(e store.Entry) []string {
	label := store.CategoryLabels[e.Category]
	if label == "" {
		label = string(e.Category)
	}

	var b strings.Builder
	title := e.TitleRU
	if e.TitleEN != "" {
		title = fmt.Sprintf("%s [%s]", e.TitleRU, e.TitleEN)
	}
	fmt.Fprintf(&b, "📚 %s\n%s\n", label, title)
	if e.SourceMeta != "" {
		fmt.Fprintf(&b, "Источник: %s\n", e.SourceMeta)
	}
	b.WriteString("\n")
	b.WriteString(e.Body)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "🔗 %s", e.URL)

	return splitMessage(b.String(), telegramLimit)
}

func ChoicesMessage(hits []store.SearchHit) string {
	var b strings.Builder
	b.WriteString("Нашёл несколько вариантов. Напиши номер или более точное название:\n\n")
	for i, h := range hits {
		label := store.CategoryLabels[h.Category]
		title := h.TitleRU
		if h.TitleEN != "" {
			title = fmt.Sprintf("%s [%s]", h.TitleRU, h.TitleEN)
		}
		fmt.Fprintf(&b, "%d) %s — %s\n", i+1, label, title)
	}
	return b.String()
}

func StatsMessage(counts map[store.Category]int, total int) string {
	order := []store.Category{
		store.CategoryClass,
		store.CategorySpecies,
		store.CategoryBackground,
		store.CategorySpell,
		store.CategoryItem,
		store.CategoryEquipment,
	}
	var b strings.Builder
	b.WriteString("База next.dnd.su (локально):\n\n")
	for _, c := range order {
		fmt.Fprintf(&b, "• %s: %d\n", store.CategoryLabels[c], counts[c])
	}
	fmt.Fprintf(&b, "\nВсего записей: %d", total)
	return b.String()
}

func HelpMessage() string {
	return strings.TrimSpace(`
Привет! Я личный справочник по next.dnd.su (5e24).

Как пользоваться:
• напиши название (RU или EN) — «Аура жизни», «Aura of Life», «Веер ветра»
• или отправь голосовое с названием

Команды:
/start — это сообщение
/help — справка
/stats — сколько записей в базе

Доступ только у пользователей из ALLOWED_USER_IDS.
`)
}

func splitMessage(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	var parts []string
	runes := []rune(text)
	for len(runes) > 0 {
		if len(runes) <= limit {
			parts = append(parts, string(runes))
			break
		}
		cut := limit
		window := runes[:limit]
		if i := lastIndexRune(window, '\n'); i > limit/2 {
			cut = i
		} else if i := lastIndexRune(window, ' '); i > limit/2 {
			cut = i
		}
		parts = append(parts, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
		for len(runes) > 0 && (runes[0] == '\n' || runes[0] == ' ') {
			runes = runes[1:]
		}
	}
	return parts
}

func lastIndexRune(runes []rune, target rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == target {
			return i
		}
	}
	return -1
}
