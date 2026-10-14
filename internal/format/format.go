package format

import (
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/SaynoMore22/dnd-telegram-bot/internal/store"
)

const telegramLimit = 3500

const (
	tableMarkerStart = "<<<TABLE>>>"
	tableMarkerEnd   = "<<<ENDTABLE>>>"

	boldMarkerStart = "\x01B\x01"
	boldMarkerEnd   = "\x01b\x01"

	classDataStart  = "<<<CLASSDATA>>>"
	classDataEnd    = "<<<ENDCLASSDATA>>>"
	traitsStart     = "<<<TRAITS>>>"
	traitsEnd       = "<<<ENDTRAITS>>>"
	featuresStart   = "<<<FEATURES>>>"
	featuresEnd     = "<<<ENDFEATURES>>>"
	subclassesStart = "<<<SUBCLASSES>>>"
	subclassesEnd   = "<<<ENDSUBCLASSES>>>"
	featStart       = "<<<F>>>"
	featEnd         = "<<<ENDF>>>"
	subStart        = "<<<SUB>>>"
	subEnd          = "<<<ENDSUB>>>"

	articleDataStart     = "<<<ARTICLE>>>"
	articleDataEnd       = "<<<ENDARTICLE>>>"
	articleIntroStart    = "<<<INTRO>>>"
	articleIntroEnd      = "<<<ENDINTRO>>>"
	articleSectionsStart = "<<<ARTSECTIONS>>>"
	articleSectionsEnd   = "<<<ENDARTSECTIONS>>>"
	articleSectionStart  = "<<<AS>>>"
	articleSectionEnd    = "<<<ENDAS>>>"
)

// Button — inline-кнопка Telegram; Data идёт в callback_query.Data.
type Button struct {
	Text string
	Data string
}

type Message struct {
	Text      string
	ParseHTML bool
	Buttons   [][]Button
}

// editionBadge помечает записи редакции 2014 в заголовке — 2024 считается основной и
// маркером не выделяется.
func editionBadge(edition string) string {
	if edition == store.Edition2014 {
		return " <i>(редакция 2014)</i>"
	}
	return ""
}

// EditionSwitchButton формирует кнопку перехода к записи в другой редакции.
func EditionSwitchButton(counterpartID int64, counterpartEdition string) Button {
	label := "📗 Версия 2024"
	if counterpartEdition == store.Edition2014 {
		label = "🕰 Версия 2014"
	}
	return Button{Text: label, Data: "ed:" + strconv.FormatInt(counterpartID, 10)}
}

func EntryMessages(e store.Entry) []Message {
	label := store.CategoryLabels[e.Category]
	if label == "" {
		label = string(e.Category)
	}

	title := e.TitleRU
	if e.TitleEN != "" {
		title = fmt.Sprintf("%s [%s]", e.TitleRU, e.TitleEN)
	}

	var head strings.Builder
	fmt.Fprintf(&head, "📚 <b>%s</b>%s\n%s\n", html.EscapeString(label), editionBadge(e.Edition), html.EscapeString(title))
	if e.SourceMeta != "" {
		fmt.Fprintf(&head, "Источник: %s\n", html.EscapeString(e.SourceMeta))
	}
	head.WriteString("\n")

	bodyHTML := bodyToTelegramHTML(e.Body)
	footer := "\n\n🔗 " + html.EscapeString(e.URL)

	full := head.String() + bodyHTML + footer
	parts := splitHTMLAware(full, telegramLimit)
	out := make([]Message, 0, len(parts))
	for _, p := range parts {
		out = append(out, Message{Text: p, ParseHTML: true})
	}
	return out
}

func bodyToTelegramHTML(body string) string {
	var b strings.Builder
	rest := body
	for {
		start := strings.Index(rest, tableMarkerStart)
		if start < 0 {
			b.WriteString(escapePreservingNewlines(rest))
			break
		}
		b.WriteString(escapePreservingNewlines(rest[:start]))
		rest = rest[start+len(tableMarkerStart):]
		end := strings.Index(rest, tableMarkerEnd)
		if end < 0 {
			b.WriteString(escapePreservingNewlines(rest))
			break
		}
		table := strings.TrimSpace(rest[:end])
		rest = rest[end+len(tableMarkerEnd):]
		b.WriteString("\n<pre>")
		b.WriteString(html.EscapeString(table))
		b.WriteString("</pre>\n")
	}
	return b.String()
}

func escapePreservingNewlines(s string) string {
	return htmlEscapeBody(s)
}

// htmlEscapeBody экранирует текст под HTML parse_mode Telegram и превращает наши
// внутренние маркеры полужирного (из article-body__feature-name / <b>/<strong> на сайте)
// в настоящие <b>...</b>.
func htmlEscapeBody(s string) string {
	s = html.EscapeString(s)
	s = strings.ReplaceAll(s, boldMarkerStart, "<b>")
	s = strings.ReplaceAll(s, boldMarkerEnd, "</b>")
	return s
}

// ClassFeature — умение класса/подкласса на определённом уровне.
type ClassFeature struct {
	Level int
	Title string
	Body  string
}

type ClassSubclass struct {
	Name     string
	Features []ClassFeature
}

// ClassData — структурированное содержимое страницы класса, собранное scrape-пакетом
// (см. internal/scrape.buildClassBody) и упакованное в Entry.Body спецмаркерами.
type ClassData struct {
	Traits     [][2]string
	Table      string
	Features   []ClassFeature
	Subclasses []ClassSubclass
}

// ParseClassData разбирает Entry.Body класса обратно в структуру. ok=false, если
// запись не в новом структурированном формате (например, старые данные до обновления
// парсера, или необычная разметка страницы) — тогда нужно использовать EntryMessages.
func ParseClassData(body string) (ClassData, bool) {
	var cd ClassData
	if !strings.Contains(body, classDataStart) {
		return cd, false
	}
	inner := between(body, classDataStart, classDataEnd)

	if t := between(inner, traitsStart, traitsEnd); t != "" {
		for _, line := range strings.Split(strings.TrimSpace(t), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			if len(parts) == 2 {
				cd.Traits = append(cd.Traits, [2]string{parts[0], parts[1]})
			}
		}
	}

	cd.Table = strings.TrimSpace(between(inner, tableMarkerStart, tableMarkerEnd))

	cd.Features = parseFeatureList(between(inner, featuresStart, featuresEnd))

	if s := between(inner, subclassesStart, subclassesEnd); s != "" {
		rest := s
		for {
			i := strings.Index(rest, subStart)
			if i < 0 {
				break
			}
			rest = rest[i+len(subStart):]
			j := strings.Index(rest, subEnd)
			if j < 0 {
				break
			}
			block := rest[:j]
			rest = rest[j+len(subEnd):]
			nl := strings.Index(block, "\n")
			var name, rem string
			if nl >= 0 {
				name = strings.TrimSpace(block[:nl])
				rem = block[nl+1:]
			} else {
				name = strings.TrimSpace(block)
			}
			if name == "" {
				continue
			}
			cd.Subclasses = append(cd.Subclasses, ClassSubclass{
				Name:     name,
				Features: parseFeatureList(rem),
			})
		}
	}

	return cd, true
}

func parseFeatureList(s string) []ClassFeature {
	var out []ClassFeature
	rest := s
	for {
		i := strings.Index(rest, featStart)
		if i < 0 {
			break
		}
		rest = rest[i+len(featStart):]
		j := strings.Index(rest, featEnd)
		if j < 0 {
			break
		}
		block := rest[:j]
		rest = rest[j+len(featEnd):]
		nl := strings.Index(block, "\n")
		if nl < 0 {
			continue
		}
		head := block[:nl]
		body := strings.TrimSpace(block[nl+1:])
		parts := strings.SplitN(head, "|", 2)
		if len(parts) != 2 {
			continue
		}
		level, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		out = append(out, ClassFeature{Level: level, Title: strings.TrimSpace(parts[1]), Body: body})
	}
	return out
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return s
	}
	return s[:j]
}

// ClassOverviewMessage — компактная карточка класса: заголовок, ключевые атрибуты
// и кнопки для перехода к таблице умений / умениям по уровням / подклассам, вместо
// того чтобы сразу вываливать весь текст класса несколькими сообщениями подряд.
func ClassOverviewMessage(e store.Entry, cd ClassData) Message {
	title := e.TitleRU
	if e.TitleEN != "" {
		title = fmt.Sprintf("%s [%s]", e.TitleRU, e.TitleEN)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📚 <b>Класс</b>%s\n%s\n", editionBadge(e.Edition), html.EscapeString(title))
	if e.SourceMeta != "" {
		fmt.Fprintf(&b, "Источник: %s\n", html.EscapeString(e.SourceMeta))
	}
	b.WriteString("\n")
	for _, t := range cd.Traits {
		fmt.Fprintf(&b, "• <b>%s:</b> %s\n", html.EscapeString(t[0]), htmlEscapeBody(t[1]))
	}

	idStr := strconv.FormatInt(e.ID, 10)
	var rows [][]Button
	if cd.Table != "" {
		rows = append(rows, []Button{{Text: "📊 Таблица умений", Data: "cf:" + idStr + ":table"}})
	}
	if len(cd.Features) > 0 {
		rows = append(rows, []Button{{Text: fmt.Sprintf("⚡ Умения по уровням (%d)", len(cd.Features)), Data: "cf:" + idStr + ":feat"}})
	}
	if len(cd.Subclasses) > 0 {
		rows = append(rows, []Button{{Text: fmt.Sprintf("🎭 Подклассы (%d)", len(cd.Subclasses)), Data: "cf:" + idStr + ":sublist"}})
	}

	b.WriteString("\n🔗 " + html.EscapeString(e.URL))

	return Message{Text: strings.TrimSpace(b.String()), ParseHTML: true, Buttons: rows}
}

// ClassTableMessage — таблица прогрессии класса по уровням отдельным сообщением.
func ClassTableMessage(cd ClassData) Message {
	text := "📊 <b>Таблица умений</b>\n<pre>" + html.EscapeString(cd.Table) + "</pre>"
	return Message{Text: text, ParseHTML: true}
}

// ClassFeaturesMessages — умения основного класса по уровням, разбитые на сообщения
// в пределах лимита Telegram.
func ClassFeaturesMessages(cd ClassData) []Message {
	return featureListMessages("⚡ <b>Умения по уровням</b>", cd.Features)
}

// ClassSubclassListMessage — список подклассов кнопками.
func ClassSubclassListMessage(e store.Entry, cd ClassData) Message {
	idStr := strconv.FormatInt(e.ID, 10)
	var rows [][]Button
	var row []Button
	for i, s := range cd.Subclasses {
		row = append(row, Button{Text: s.Name, Data: fmt.Sprintf("cf:%s:sub:%d", idStr, i)})
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return Message{Text: "🎭 Выбери подкласс:", Buttons: rows}
}

// ClassSubclassMessages — умения конкретного подкласса по уровням.
func ClassSubclassMessages(cd ClassData, idx int) []Message {
	if idx < 0 || idx >= len(cd.Subclasses) {
		return nil
	}
	sub := cd.Subclasses[idx]
	header := fmt.Sprintf("🎭 <b>Подкласс: %s</b>", html.EscapeString(sub.Name))
	return featureListMessages(header, sub.Features)
}

func featureListMessages(header string, features []ClassFeature) []Message {
	var body strings.Builder
	body.WriteString(header + "\n\n")
	for _, f := range features {
		if f.Title != "" {
			fmt.Fprintf(&body, "<b>Уровень %d: %s</b>\n", f.Level, html.EscapeString(f.Title))
		}
		body.WriteString(bodyToTelegramHTML(f.Body))
		body.WriteString("\n\n")
	}
	full := strings.TrimSpace(body.String())
	parts := splitHTMLAware(full, telegramLimit)
	out := make([]Message, 0, len(parts))
	for _, p := range parts {
		out = append(out, Message{Text: p, ParseHTML: true})
	}
	return out
}

// ArticleSection — логический подпункт длинной статьи (см. internal/scrape.buildSectionedBody).
type ArticleSection struct {
	Title string
	Body  string
}

// ArticleData — статья, разбитая на вступление + подпункты по заголовкам, вместо одной
// простыни текста (см. категорию «Игровые механики»).
type ArticleData struct {
	Intro    string
	Sections []ArticleSection
}

// ParseArticleData разбирает Entry.Body обратно в структуру. ok=false — запись не в
// секционированном формате (короткая статья без подходящих заголовков, либо старые
// данные до обновления парсера) — нужно показывать через EntryMessages как обычно.
func ParseArticleData(body string) (ArticleData, bool) {
	var ad ArticleData
	if !strings.Contains(body, articleDataStart) {
		return ad, false
	}
	inner := between(body, articleDataStart, articleDataEnd)
	ad.Intro = strings.TrimSpace(between(inner, articleIntroStart, articleIntroEnd))

	rest := between(inner, articleSectionsStart, articleSectionsEnd)
	for {
		i := strings.Index(rest, articleSectionStart)
		if i < 0 {
			break
		}
		rest = rest[i+len(articleSectionStart):]
		j := strings.Index(rest, articleSectionEnd)
		if j < 0 {
			break
		}
		block := rest[:j]
		rest = rest[j+len(articleSectionEnd):]
		nl := strings.Index(block, "\n")
		if nl < 0 {
			continue
		}
		title := strings.TrimSpace(block[:nl])
		secBody := strings.TrimSpace(block[nl+1:])
		if title == "" {
			continue
		}
		ad.Sections = append(ad.Sections, ArticleSection{Title: title, Body: secBody})
	}
	return ad, true
}

// ArticleOverviewMessage — вступление статьи + кнопки на её подпункты, вместо того чтобы
// сразу вываливать всю статью несколькими сообщениями подряд.
func ArticleOverviewMessage(e store.Entry, ad ArticleData) Message {
	title := e.TitleRU
	if e.TitleEN != "" {
		title = fmt.Sprintf("%s [%s]", e.TitleRU, e.TitleEN)
	}
	label := store.CategoryLabels[e.Category]
	if label == "" {
		label = string(e.Category)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📚 <b>%s</b>%s\n%s\n", html.EscapeString(label), editionBadge(e.Edition), html.EscapeString(title))
	if e.SourceMeta != "" {
		fmt.Fprintf(&b, "Источник: %s\n", html.EscapeString(e.SourceMeta))
	}
	b.WriteString("\n")
	if ad.Intro != "" {
		b.WriteString(bodyToTelegramHTML(ad.Intro))
		b.WriteString("\n")
	}
	b.WriteString("\n🔗 " + html.EscapeString(e.URL))

	idStr := strconv.FormatInt(e.ID, 10)
	var rows [][]Button
	for i, s := range ad.Sections {
		rows = append(rows, []Button{{Text: s.Title, Data: fmt.Sprintf("af:%s:%d", idStr, i)}})
	}

	return Message{Text: strings.TrimSpace(b.String()), ParseHTML: true, Buttons: rows}
}

// ArticleSectionMessages — содержимое одного подпункта статьи.
func ArticleSectionMessages(ad ArticleData, idx int) []Message {
	if idx < 0 || idx >= len(ad.Sections) {
		return nil
	}
	s := ad.Sections[idx]
	header := fmt.Sprintf("📖 <b>%s</b>\n\n", html.EscapeString(s.Title))
	full := strings.TrimSpace(header + bodyToTelegramHTML(s.Body))
	parts := splitHTMLAware(full, telegramLimit)
	out := make([]Message, 0, len(parts))
	for _, p := range parts {
		out = append(out, Message{Text: p, ParseHTML: true})
	}
	return out
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
		store.CategoryFeat,
		store.CategoryFormulas,
		store.CategoryMechanics,
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
Привет! Я личный справочник по next.dnd.su (5e24) и dnd.su (5e14).

Как пользоваться:
• напиши название (RU или EN) — «Аура жизни», «Aura of Life», «Веер ветра»
• или отправь голосовое с названием
• или жми /menu и брожу по разделам кнопками

Команды:
/start, /menu — меню разделов
/help — справка
/stats — сколько записей в базе

Доступ только у пользователей из ALLOWED_USER_IDS.
`)
}

// menuCategories — разделы, доступные для просмотра кнопками из /menu. Показываем
// только основную редакцию (2024) — 2014-версия конкретной записи достаётся через
// кнопку переключения редакции на самой карточке.
var menuCategories = []store.Category{
	store.CategoryClass,
	store.CategorySpecies,
	store.CategoryBackground,
	store.CategorySpell,
	store.CategoryItem,
	store.CategoryEquipment,
	store.CategoryFeat,
	store.CategoryFormulas,
	store.CategoryMechanics,
}

var categoryMenuLabels = map[store.Category]string{
	store.CategoryClass:      "🧙 Классы",
	store.CategorySpecies:    "🧝 Виды",
	store.CategoryBackground: "📜 Предыстории",
	store.CategorySpell:      "✨ Заклинания",
	store.CategoryItem:       "💎 Магические предметы",
	store.CategoryEquipment:  "⚔️ Снаряжение",
	store.CategoryFeat:       "🎖 Черты",
	store.CategoryFormulas:   "📐 Основные формулы",
	store.CategoryMechanics:  "⚙️ Игровые механики",
}

// directListMax — до скольких записей в разделе показываем сразу постранично, без
// промежуточного выбора буквы.
const directListMax = 25

// NeedsLetterIndex — нужен ли для раздела такого размера промежуточный алфавитный индекс.
func NeedsLetterIndex(count int) bool {
	return count > directListMax
}

// menuPageSize — сколько карточек-кнопок на одной странице списка.
const menuPageSize = 10

// MainMenuMessage — стартовое меню разделов.
func MainMenuMessage() Message {
	var rows [][]Button
	for _, c := range menuCategories {
		rows = append(rows, []Button{{Text: categoryMenuLabels[c], Data: "mn:cat:" + string(c)}})
	}
	return Message{Text: "Выбери раздел:", Buttons: rows}
}

// CategoryEntryLetter возвращает заглавную первую букву русского названия — используется
// и для построения алфавитного индекса, и для фильтрации списка по нему.
func CategoryEntryLetter(titleRU string) string {
	r := []rune(strings.TrimSpace(titleRU))
	if len(r) == 0 {
		return "?"
	}
	return strings.ToUpper(string(r[0]))
}

// CategoryLettersMessage — алфавитный индекс для больших разделов.
func CategoryLettersMessage(category store.Category, items []store.TitleEntry) Message {
	counts := make(map[string]int)
	var letters []string
	for _, it := range items {
		l := CategoryEntryLetter(it.TitleRU)
		if counts[l] == 0 {
			letters = append(letters, l)
		}
		counts[l]++
	}
	sort.Strings(letters)

	var rows [][]Button
	var row []Button
	for _, l := range letters {
		row = append(row, Button{Text: fmt.Sprintf("%s (%d)", l, counts[l]), Data: "mn:page:" + string(category) + ":" + l + ":0"})
		if len(row) == 5 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []Button{{Text: "🏠 Разделы", Data: "mn:home"}})

	label := categoryMenuLabels[category]
	if label == "" {
		label = store.CategoryLabels[category]
	}
	return Message{Text: fmt.Sprintf("%s — выбери букву (всего %d):", label, len(items)), Buttons: rows}
}

// CategoryPageMessage — страница списка карточек-кнопок (letter="*" — без фильтра по
// букве, для небольших разделов, куда идём сразу из главного меню).
func CategoryPageMessage(category store.Category, letter string, items []store.TitleEntry, page int) Message {
	filtered := items
	if letter != "*" {
		filtered = make([]store.TitleEntry, 0, len(items))
		for _, it := range items {
			if CategoryEntryLetter(it.TitleRU) == letter {
				filtered = append(filtered, it)
			}
		}
	}

	start := page * menuPageSize
	if start >= len(filtered) {
		start = 0
		page = 0
	}
	end := start + menuPageSize
	if end > len(filtered) {
		end = len(filtered)
	}

	var rows [][]Button
	for _, it := range filtered[start:end] {
		title := it.TitleRU
		if it.TitleEN != "" {
			title = fmt.Sprintf("%s [%s]", it.TitleRU, it.TitleEN)
		}
		rows = append(rows, []Button{{Text: title, Data: "mn:entry:" + strconv.FormatInt(it.ID, 10)}})
	}

	var nav []Button
	if page > 0 {
		nav = append(nav, Button{Text: "« Назад", Data: fmt.Sprintf("mn:page:%s:%s:%d", category, letter, page-1)})
	}
	if end < len(filtered) {
		nav = append(nav, Button{Text: "Вперёд »", Data: fmt.Sprintf("mn:page:%s:%s:%d", category, letter, page+1)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}

	var back []Button
	if letter != "*" {
		back = append(back, Button{Text: "🔤 Буквы", Data: "mn:cat:" + string(category)})
	}
	back = append(back, Button{Text: "🏠 Разделы", Data: "mn:home"})
	rows = append(rows, back)

	label := categoryMenuLabels[category]
	if label == "" {
		label = store.CategoryLabels[category]
	}
	text := fmt.Sprintf("%s — стр. %d из %d (%d записей):", label, page+1, pageCount(len(filtered)), len(filtered))
	return Message{Text: text, Buttons: rows}
}

func pageCount(n int) int {
	if n == 0 {
		return 1
	}
	return (n + menuPageSize - 1) / menuPageSize
}

func splitHTMLAware(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	// режем по абзацам / pre-блокам, не ломая <pre>
	chunks := splitKeepingPre(text)
	var parts []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			parts = append(parts, s)
		}
		cur.Reset()
		curLen = 0
	}
	for _, chunk := range chunks {
		n := utf8.RuneCountInString(chunk)
		if curLen > 0 && curLen+n > limit {
			flush()
		}
		if n > limit {
			// аварийно режем огромный pre
			runes := []rune(chunk)
			for len(runes) > 0 {
				take := limit
				if take > len(runes) {
					take = len(runes)
				}
				parts = append(parts, string(runes[:take]))
				runes = runes[take:]
			}
			continue
		}
		cur.WriteString(chunk)
		curLen += n
	}
	flush()
	return parts
}

func splitKeepingPre(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "<pre>")
		if i < 0 {
			if s != "" {
				out = append(out, splitParagraphs(s)...)
			}
			return out
		}
		if i > 0 {
			out = append(out, splitParagraphs(s[:i])...)
		}
		j := strings.Index(s[i:], "</pre>")
		if j < 0 {
			out = append(out, s[i:])
			return out
		}
		j = i + j + len("</pre>")
		out = append(out, s[i:j])
		s = s[j:]
	}
}

func splitParagraphs(s string) []string {
	parts := strings.Split(s, "\n\n")
	var out []string
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p+"\n\n")
	}
	return out
}
