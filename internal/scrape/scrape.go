package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SaynoMore22/dnd-telegram-bot/internal/store"
	"golang.org/x/net/html"
)

type CategorySpec struct {
	Category store.Category
	Path     string
	// LinkPrefix — префикс, с которого должны начинаться ссылки на карточки в режимах
	// href_numbered/href_slug. Нужен, когда список тянется с одного URL (например, AJAX
	// piece-эндпоинт), а сами карточки лежат по другому пути. Пусто — берём Path.
	LinkPrefix string
	Mode       string // list_json | href_numbered | href_slug | single
}

// Categories — редакция 2024 (next.dnd.su), основной источник бота.
var Categories = []CategorySpec{
	{Category: store.CategoryClass, Path: "/class/", Mode: "href_slug"},
	{Category: store.CategorySpecies, Path: "/species/", Mode: "href_slug"},
	{Category: store.CategoryBackground, Path: "/backgrounds/", Mode: "href_numbered"},
	{Category: store.CategorySpell, Path: "/spells/", Mode: "list_json"},
	{Category: store.CategoryItem, Path: "/items/", Mode: "list_json"},
	{Category: store.CategoryEquipment, Path: "/equipment/", Mode: "href_numbered"},
	{Category: store.CategoryFeat, Path: "/feats/", Mode: "list_json"},
	{Category: store.CategoryFormulas, Path: "/newbie/basics", Mode: "single"},
	{Category: store.CategoryMechanics, Path: "/mechanics/", Mode: "href_slug"},
}

// Categories2014 — редакция 2014 (dnd.su, классический справочник). Список короче: тянем
// только то, что реально пересекается по актуальности с 2024-редакцией.
var Categories2014 = []CategorySpec{
	{Category: store.CategorySpell, Path: "/piece/spells/index-list/", LinkPrefix: "/spells", Mode: "list_json"},
	{Category: store.CategoryItem, Path: "/piece/items/index-list/", LinkPrefix: "/items", Mode: "href_numbered"},
	{Category: store.CategoryFeat, Path: "/feats/", Mode: "href_numbered"},
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Delay      time.Duration
	// Edition — какой редакцией помечать сохранённые записи (store.Edition2024/2014).
	Edition string
}

type listCard struct {
	Title   string `json:"title"`
	TitleEN string `json:"title_en"`
	Link    string `json:"link"`
}

type listPayload struct {
	Cards []listCard `json:"cards"`
}

type ProgressFunc func(msg string)

func New(baseURL string) *Client {
	return NewEdition(baseURL, store.Edition2024)
}

func NewEdition(baseURL, edition string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 45 * time.Second,
		},
		Delay:   350 * time.Millisecond,
		Edition: edition,
	}
}

func (c *Client) IngestAll(ctx context.Context, st *store.Store, progress ProgressFunc) error {
	return c.IngestCategories(ctx, st, Categories, progress)
}

func (c *Client) IngestCategories(ctx context.Context, st *store.Store, cats []CategorySpec, progress ProgressFunc) error {
	for _, cat := range cats {
		if err := c.IngestCategory(ctx, st, cat, progress); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) IngestCategory(ctx context.Context, st *store.Store, cat CategorySpec, progress ProgressFunc) error {
	if progress != nil {
		progress(fmt.Sprintf("Раздел %s: собираю список…", cat.Category))
	}

	links, err := c.listLinks(ctx, cat)
	if err != nil {
		return fmt.Errorf("%s list: %w", cat.Category, err)
	}
	if progress != nil {
		progress(fmt.Sprintf("Раздел %s: найдено %d страниц", cat.Category, len(links)))
	}

	for i, item := range links {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		entry, err := c.fetchEntry(ctx, cat.Category, item)
		if err != nil {
			if progress != nil {
				progress(fmt.Sprintf("пропуск %s: %v", item.Link, err))
			}
			continue
		}
		if err := st.Upsert(ctx, entry); err != nil {
			return err
		}
		if progress != nil && (i+1)%25 == 0 {
			progress(fmt.Sprintf("%s: %d/%d", cat.Category, i+1, len(links)))
		}
		time.Sleep(c.Delay)
	}

	if progress != nil {
		progress(fmt.Sprintf("Раздел %s: готово (%d)", cat.Category, len(links)))
	}
	return nil
}

type listItem struct {
	Link    string
	TitleRU string
	TitleEN string
}

func (c *Client) listLinks(ctx context.Context, cat CategorySpec) ([]listItem, error) {
	if cat.Mode == "single" {
		// Одна фиксированная статья — списка страниц нет, сама Path и есть ссылка на неё.
		return []listItem{{Link: cat.Path}}, nil
	}

	body, err := c.get(ctx, cat.Path)
	if err != nil {
		return nil, err
	}

	linkPath := cat.LinkPrefix
	if linkPath == "" {
		linkPath = cat.Path
	}

	switch cat.Mode {
	case "list_json":
		return parseWindowList(body)
	case "href_numbered":
		base := regexp.QuoteMeta(strings.TrimSuffix(linkPath, "/"))
		re := regexp.MustCompile(`href=["'](` + base + `/\d+-[a-zA-Z0-9\-]+)/?["']`)
		return uniqueLinks(re.FindAllStringSubmatch(body, -1)), nil
	case "href_slug":
		base := regexp.QuoteMeta(strings.TrimSuffix(linkPath, "/"))
		re := regexp.MustCompile(`href=["'](` + base + `/[a-z][a-zA-Z0-9\-]+)/?["']`)
		return uniqueLinks(re.FindAllStringSubmatch(body, -1)), nil
	default:
		return nil, fmt.Errorf("unknown mode %s", cat.Mode)
	}
}

func parseWindowList(body string) ([]listItem, error) {
	const marker = "window.LIST = "
	idx := strings.Index(body, marker)
	if idx < 0 {
		return nil, fmt.Errorf("window.LIST not found")
	}
	raw := strings.TrimSpace(body[idx+len(marker):])
	dec := json.NewDecoder(strings.NewReader(raw))
	var payload listPayload
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse LIST json: %w", err)
	}

	out := make([]listItem, 0, len(payload.Cards))
	seen := make(map[string]struct{})
	for _, card := range payload.Cards {
		link := strings.TrimSpace(card.Link)
		if link == "" {
			continue
		}
		if !strings.HasPrefix(link, "/") {
			link = "/" + link
		}
		if _, ok := seen[link]; ok {
			continue
		}
		seen[link] = struct{}{}
		out = append(out, listItem{
			Link:    link,
			TitleRU: strings.TrimSpace(card.Title),
			TitleEN: strings.TrimSpace(card.TitleEN),
		})
	}
	return out, nil
}

func uniqueLinks(matches [][]string) []listItem {
	seen := make(map[string]struct{})
	var out []listItem
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		link := m[1]
		if _, ok := seen[link]; ok {
			continue
		}
		seen[link] = struct{}{}
		out = append(out, listItem{Link: link})
	}
	return out
}

func (c *Client) fetchEntry(ctx context.Context, category store.Category, item listItem) (store.Entry, error) {
	body, err := c.get(ctx, item.Link)
	if err != nil {
		return store.Entry{}, err
	}

	titleRU, titleEN, sourceMeta, textBody, err := parseCard(body, category)
	if err != nil {
		return store.Entry{}, err
	}
	if item.TitleRU != "" && titleRU == "" {
		titleRU = item.TitleRU
	}
	if item.TitleEN != "" && titleEN == "" {
		titleEN = item.TitleEN
	}
	if titleRU == "" {
		return store.Entry{}, fmt.Errorf("empty title for %s", item.Link)
	}

	slug := strings.Trim(item.Link, "/")
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}

	absURL, err := url.JoinPath(c.BaseURL, item.Link)
	if err != nil {
		absURL = c.BaseURL + item.Link
	}
	if !strings.HasSuffix(absURL, "/") {
		absURL += "/"
	}

	edition := c.Edition
	if edition == "" {
		edition = store.Edition2024
	}

	summary := firstParagraph(textBody)
	return store.Entry{
		Category:   category,
		Slug:       slug,
		Edition:    edition,
		URL:        absURL,
		TitleRU:    titleRU,
		TitleEN:    titleEN,
		Summary:    summary,
		Body:       textBody,
		SourceMeta: sourceMeta,
	}, nil
}

func (c *Client) get(ctx context.Context, path string) (string, error) {
	u := path
	if strings.HasPrefix(path, "/") {
		u = c.BaseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "dnd-telegram-bot/1.0 (+personal offline mirror)")
	req.Header.Set("Accept-Language", "ru,en;q=0.8")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

var (
	reTitleBracket = regexp.MustCompile(`^\s*(.+?)\s*\[([^\]]+)\]\s*$`)
	reSpaces       = regexp.MustCompile(`[ \t\xA0]+`)
)

func parseCard(pageHTML string, category store.Category) (titleRU, titleEN, sourceMeta, body string, err error) {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return "", "", "", "", err
	}

	card := findElement(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "div" &&
			hasClass(n, "card") && hasClass(n, "paper-1") && !hasClass(n, "note")
	})
	if card == nil {
		card = findElement(doc, func(n *html.Node) bool {
			return n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "card") && hasClass(n, "typoable") && !hasClass(n, "note")
		})
	}
	if card == nil {
		return "", "", "", "", fmt.Errorf("card not found")
	}

	removeNodes(card, shouldDropNode)

	titleNode := findElement(card, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "card-title")
	})
	sourceMeta = collectSources(titleNode)
	// На dnd.su (5e14) плашки source-plaque несут видимый текст ("PH14"/"PH24") прямо
	// внутри заголовка — вырезаем их после того, как собрали источники из title-атрибута,
	// иначе он попадёт в название карточки.
	removeNodes(titleNode, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "source-plaque")
	})
	rawTitle := strings.TrimSpace(textContent(titleNode))
	rawTitle = strings.ReplaceAll(rawTitle, "Распечатать", "")
	rawTitle = strings.TrimSpace(rawTitle)
	titleRU, titleEN = splitTitle(rawTitle)

	bodyNode := findElement(card, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "card__body") && hasClass(n, "new-article")
	})
	if bodyNode == nil {
		bodyNode = findElement(card, func(n *html.Node) bool {
			return n.Type == html.ElementNode && hasClass(n, "card__body")
		})
	}
	if category == store.CategoryClass {
		if cb, ok := buildClassBody(bodyNode); ok {
			body = cb
		}
	}
	if body == "" && category == store.CategoryMechanics {
		if ab, ok := buildSectionedBody(bodyNode); ok {
			body = ab
		}
	}
	if body == "" {
		body = stripTrailingJunk(normalizeText(textContent(bodyNode)))
	}
	if body == "" {
		return "", "", "", "", fmt.Errorf("empty body")
	}
	return titleRU, titleEN, sourceMeta, body, nil
}

func shouldDropNode(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "script", "style", "noscript", "svg", "template":
		return true
	}
	dropClasses := []string{
		"short",
		"card-menu",
		"comments-block",
		"comment__entry",
		"gallery",
		"toUp",
	}
	for _, c := range dropClasses {
		if hasClass(n, c) {
			return true
		}
	}
	// multiforo используется и для контента классов — режем только комментарии
	if hasClass(n, "multiforo") && (getAttr(n, "data-page") == "comments" || getAttr(n, "caption") == "Комментарии") {
		return true
	}
	if getAttr(n, "data-page") == "comments" {
		return true
	}
	if getAttr(n, "caption") == "Комментарии" {
		return true
	}
	return false
}

func removeNodes(root *html.Node, drop func(*html.Node) bool) {
	if root == nil {
		return
	}
	var next *html.Node
	for c := root.FirstChild; c != nil; c = next {
		next = c.NextSibling
		if drop(c) {
			root.RemoveChild(c)
			continue
		}
		removeNodes(c, drop)
	}
}

func stripTrailingJunk(body string) string {
	cutMarkers := []string{
		"\nКомментарии\n",
		"\nwindow.commentsAccess",
		"\nПоказывать следующие подклассы",
		"\nX\n\nПоказывать",
	}
	cut := -1
	for _, m := range cutMarkers {
		if i := strings.Index(body, m); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut >= 0 {
		body = strings.TrimSpace(body[:cut])
	}
	return body
}

func splitTitle(raw string) (ru, en string) {
	if m := reTitleBracket.FindStringSubmatch(raw); len(m) == 3 {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
	}
	return strings.TrimSpace(raw), ""
}

func collectSources(titleNode *html.Node) string {
	if titleNode == nil {
		return ""
	}
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && hasClass(n, "source-plaque") {
			if t := getAttr(n, "title"); t != "" {
				parts = append(parts, t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(titleNode)
	return strings.Join(uniqueStrings(parts), "; ")
}

func firstParagraph(body string) string {
	parts := strings.Split(body, "\n\n")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			if len(p) > 280 {
				return p[:277] + "…"
			}
			return p
		}
	}
	return ""
}

func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\u00a0", " ")

	var b strings.Builder
	rest := s
	for {
		start := strings.Index(rest, tableMarkerStart)
		if start < 0 {
			b.WriteString(normalizeProse(rest))
			break
		}
		b.WriteString(normalizeProse(rest[:start]))
		b.WriteString(tableMarkerStart)
		rest = rest[start+len(tableMarkerStart):]
		end := strings.Index(rest, tableMarkerEnd)
		if end < 0 {
			b.WriteString(rest)
			break
		}
		// таблицы не трогаем: пробелы = выравнивание колонок
		b.WriteString(rest[:end])
		b.WriteString(tableMarkerEnd)
		rest = rest[end+len(tableMarkerEnd):]
	}
	return strings.TrimSpace(b.String())
}

func normalizeProse(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(reSpaces.ReplaceAllString(line, " "))
		if line == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func findElement(n *html.Node, pred func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	if pred(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, pred); found != nil {
			return found
		}
	}
	return nil
}

func textContent(n *html.Node) string {
	return textContentOpt(n, 0)
}

// writeHeading пишет заголовок отдельно от общего рекурсивного walk: текст заголовка в
// исходном HTML отформатирован с переносами строк и отступами ("\n\t\tЗаголовок\n\t"),
// а маркеру секции нужен чистый однострочный текст — иначе перенос строки внутри
// заголовка обрежет его раньше, чем начнётся тело секции.
func writeHeading(b *strings.Builder, node *html.Node, asSection bool, level string) {
	title := strings.Join(strings.Fields(textContent(node)), " ")
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	if asSection {
		b.WriteString(sectionMarkerStart + level + "|" + title + "\n")
	} else {
		b.WriteString(boldMarkerStart + title + boldMarkerEnd + "\n")
	}
}

// textContentOpt — как textContent, но при sectionLevel 3 или 4 дополнительно превращает
// заголовки h3.article-body__header_small / h4.article-body__header_tiny выбранного уровня
// в маркеры секций (sectionMarkerStart) для последующей разбивки статьи на подпункты;
// заголовки другого уровня при этом просто выделяются жирным (как вложенные подзаголовки).
func textContentOpt(n *html.Node, sectionLevel int) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
			return
		}
		if node.Type != html.ElementNode {
			return
		}
		if node.Data == "table" {
			ascii := formatHTMLTable(node)
			if strings.TrimSpace(ascii) == "" {
				return
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(tableMarkerStart)
			b.WriteString("\n")
			b.WriteString(ascii)
			b.WriteString("\n")
			b.WriteString(tableMarkerEnd)
			b.WriteString("\n")
			return
		}
		if sectionLevel > 0 && node.Data == "h3" && hasClass(node, "article-body__header_small") {
			writeHeading(&b, node, sectionLevel == 3, "3")
			return
		}
		if sectionLevel > 0 && node.Data == "h4" && hasClass(node, "article-body__header_tiny") {
			writeHeading(&b, node, sectionLevel == 4, "4")
			return
		}
		if node.Data == "b" || node.Data == "strong" ||
			(node.Data == "span" && hasClass(node, "article-body__feature-name")) {
			b.WriteString(boldMarkerStart)
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			b.WriteString(boldMarkerEnd)
			return
		}
		switch node.Data {
		case "br":
			b.WriteString(" ")
		case "p", "li", "h1", "h2", "h3", "h4", "div":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		switch node.Data {
		case "p", "li", "h1", "h2", "h3", "h4":
			b.WriteString("\n")
		}
	}
	walk(n)
	return b.String()
}

const (
	tableMarkerStart = "<<<TABLE>>>"
	tableMarkerEnd   = "<<<ENDTABLE>>>"

	boldMarkerStart = "B"
	boldMarkerEnd   = "b"

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

	sectionMarkerStart   = "<<<SEC>>>"
	articleDataStart     = "<<<ARTICLE>>>"
	articleDataEnd       = "<<<ENDARTICLE>>>"
	articleIntroStart    = "<<<INTRO>>>"
	articleIntroEnd      = "<<<ENDINTRO>>>"
	articleSectionsStart = "<<<ARTSECTIONS>>>"
	articleSectionsEnd   = "<<<ENDARTSECTIONS>>>"
	articleSectionStart  = "<<<AS>>>"
	articleSectionEnd    = "<<<ENDAS>>>"
)

type classFeature struct {
	Level int
	Title string
	Body  string
}

// buildClassBody собирает структурированное представление страницы класса:
// ключевые атрибуты, таблицу умений по уровням, умения основного класса и подклассы.
// Возвращает ok=false, если страница не соответствует ожидаемой разметке — тогда
// вызывающий код должен использовать обычный плоский текст.
// buildSectionedBody разбивает длинную статью (например, «Игровые механики») на
// логические подпункты по заголовкам h3.article-body__header_small /
// h4.article-body__header_tiny — по аналогии с тем, как умения класса разбиты на
// отдельные кнопки вместо одной простыни текста.
//
// Уровень заголовка для разбивки выбирается тем, которого на странице больше одного:
// если h3 несколько — режем по h3, а одиночные h4 внутри остаются жирными подзаголовками
// внутри своей секции; если h3 только один (или нет), но несколько h4 — режем по h4.
// Если подходящих заголовков нет вовсе — возвращает ok=false, вызывающий код должен
// показать статью как обычный плоский текст.
func buildSectionedBody(root *html.Node) (string, bool) {
	h3n := countHeadings(root, "h3", "article-body__header_small")
	h4n := countHeadings(root, "h4", "article-body__header_tiny")

	splitLevel := 0
	switch {
	case h3n > 1:
		splitLevel = 3
	case h4n > 1:
		splitLevel = 4
	default:
		return "", false
	}

	raw := textContentOpt(root, splitLevel)
	i := strings.Index(raw, sectionMarkerStart)
	if i < 0 {
		return "", false
	}
	intro := strings.TrimSpace(normalizeText(raw[:i]))
	rest := raw[i:]

	type section struct{ title, body string }
	var sections []section
	for {
		idx := strings.Index(rest, sectionMarkerStart)
		if idx < 0 {
			break
		}
		rest = rest[idx+len(sectionMarkerStart):]
		nl := strings.Index(rest, "\n")
		if nl < 0 {
			break
		}
		head := rest[:nl]
		parts := strings.SplitN(head, "|", 2)
		title := head
		if len(parts) == 2 {
			title = parts[1]
		}
		title = strings.TrimSpace(title)

		rest = rest[nl+1:]
		next := strings.Index(rest, sectionMarkerStart)
		var body string
		if next < 0 {
			body = rest
			rest = ""
		} else {
			body = rest[:next]
			rest = rest[next:]
		}
		body = strings.TrimSpace(normalizeText(body))
		if title != "" {
			sections = append(sections, section{title: title, body: body})
		}
		if rest == "" {
			break
		}
	}
	if len(sections) < 2 {
		return "", false
	}

	var b strings.Builder
	b.WriteString(articleDataStart + "\n")
	b.WriteString(articleIntroStart + "\n" + intro + "\n" + articleIntroEnd + "\n")
	b.WriteString(articleSectionsStart + "\n")
	for _, s := range sections {
		b.WriteString(articleSectionStart + s.title + "\n")
		b.WriteString(s.body)
		b.WriteString("\n" + articleSectionEnd + "\n")
	}
	b.WriteString(articleSectionsEnd + "\n")
	b.WriteString(articleDataEnd + "\n")
	return b.String(), true
}

func countHeadings(root *html.Node, tag, class string) int {
	n := 0
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == tag && hasClass(node, class) {
			n++
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return n
}

func buildClassBody(root *html.Node) (string, bool) {
	tableNode := findElement(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "table" && getAttr(n, "id") == "class-features"
	})
	if tableNode == nil {
		return "", false
	}
	tableASCII := formatHTMLTable(tableNode)

	traits := extractTraits(root)

	// Собираем узлы умений и подклассов ДО того, как extractFeatures начнёт вырезать
	// вложенные дубликаты — иначе часть дерева (в т.ч. подклассы) окажется отрезана от
	// root ещё до того, как мы до неё дойдём (см. комментарий у findAllElements).
	featureNodes := findAllElements(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "class__feature")
	})
	holderNodes := findAllElements(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "class__subclass__holder")
	})
	subFeatureNodesByHolder := make([][]*html.Node, len(holderNodes))
	for i, holder := range holderNodes {
		subFeatureNodesByHolder[i] = findAllElements(holder, func(n *html.Node) bool {
			return n.Type == html.ElementNode && hasClass(n, "class__subclass__feature")
		})
	}

	features := extractFeatures(featureNodes, "class__feature", "article-body__class-feature__head")
	if len(features) == 0 {
		return "", false
	}

	var b strings.Builder
	b.WriteString(classDataStart + "\n")

	b.WriteString(traitsStart + "\n")
	for _, t := range traits {
		fmt.Fprintf(&b, "%s|%s\n", strings.ReplaceAll(t[0], "|", "/"), strings.ReplaceAll(t[1], "|", "/"))
	}
	b.WriteString(traitsEnd + "\n")

	b.WriteString(tableMarkerStart + "\n" + tableASCII + "\n" + tableMarkerEnd + "\n")

	b.WriteString(featuresStart + "\n")
	for _, f := range features {
		writeFeature(&b, f)
	}
	b.WriteString(featuresEnd + "\n")

	b.WriteString(subclassesStart + "\n")
	for i, holder := range holderNodes {
		name := strings.TrimSpace(getAttr(holder, "data-title"))
		if name == "" {
			continue
		}
		subFeatures := extractFeatures(subFeatureNodesByHolder[i], "class__subclass__feature", "article-body__subclass-feature__head")
		b.WriteString(subStart + name + "\n")
		for _, f := range subFeatures {
			writeFeature(&b, f)
		}
		b.WriteString(subEnd + "\n")
	}
	b.WriteString(subclassesEnd + "\n")

	b.WriteString(classDataEnd + "\n")
	return b.String(), true
}

func writeFeature(b *strings.Builder, f classFeature) {
	b.WriteString(featStart)
	fmt.Fprintf(b, "%d|%s\n", f.Level, strings.ReplaceAll(f.Title, "|", "/"))
	b.WriteString(f.Body)
	b.WriteString("\n" + featEnd + "\n")
}

func extractTraits(root *html.Node) [][2]string {
	var out [][2]string
	nodes := findAllElements(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "class__core_traits__trait")
	})
	for _, n := range nodes {
		capEl := findElement(n, func(c *html.Node) bool {
			return c.Type == html.ElementNode && hasClass(c, "class__core_traits__caption")
		})
		textEl := findElement(n, func(c *html.Node) bool {
			return c.Type == html.ElementNode && hasClass(c, "class__core_traits__text")
		})
		cap := strings.TrimSpace(plainText(capEl))
		val := strings.TrimSpace(reSpaces.ReplaceAllString(plainTextKeepBreaks(textEl), " "))
		val = strings.ReplaceAll(val, " \n", "\n")
		if cap == "" {
			continue
		}
		out = append(out, [2]string{cap, val})
	}
	return out
}

// extractFeatures вытаскивает уровень/название/текст из уже собранного (до всяких мутаций
// дерева) списка узлов-умений, удаляя дублирующий заголовок и вложенные умения-дубликаты.
func extractFeatures(nodes []*html.Node, wantClass, headClass string) []classFeature {
	var out []classFeature
	for _, n := range nodes {
		level := 0
		fmt.Sscanf(getAttr(n, "data-level"), "%d", &level)
		title := strings.TrimSpace(getAttr(n, "data-title"))
		if title == "-" {
			title = ""
		}
		removeNodes(n, func(c *html.Node) bool {
			if c.Type != html.ElementNode {
				return false
			}
			if c.Data == "h3" && hasClass(c, headClass) {
				return true
			}
			// вложенные (из-за сломанной пагинации) умения обрабатываются как
			// самостоятельные записи — не дублируем их текст здесь.
			return hasClass(c, wantClass)
		})
		bodyText := strings.TrimSpace(normalizeText(textContent(n)))
		if bodyText == "" {
			continue
		}
		out = append(out, classFeature{Level: level, Title: title, Body: bodyText})
	}
	return out
}

// findAllElements собирает все узлы, подходящие под pred, по всему поддереву. Обход не
// останавливается на совпадении и продолжает спускаться в детей — на next.dnd.su
// виджет постраничной подгрузки (multiforo) закрывает теги с ошибками, из-за чего
// вторая и последующие "страницы" умений оказываются вложены внутрь последнего умения
// первой страницы, а не идут его соседями.
func findAllElements(root *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && pred(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func plainTextKeepBreaks(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		if node.Type == html.ElementNode && node.Data == "br" {
			b.WriteString("\n")
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// dropUselessColumns убирает колонки, которые на сайте заполняются через JS (в статичном
// HTML это плейсхолдер "~" в каждой строке) — они не несут информации в текстовом виде.
func dropUselessColumns(rows [][]string) [][]string {
	if len(rows) < 2 {
		return rows
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	keep := make([]bool, cols)
	for j := 0; j < cols; j++ {
		keep[j] = true
		any := false
		allTilde := true
		for i := 1; i < len(rows); i++ {
			if j >= len(rows[i]) {
				continue
			}
			v := strings.TrimSpace(rows[i][j])
			if v == "" {
				continue
			}
			any = true
			if v != "~" {
				allTilde = false
				break
			}
		}
		if any && allTilde {
			keep[j] = false
		}
	}
	out := make([][]string, len(rows))
	for i, r := range rows {
		nr := make([]string, 0, cols)
		for j, v := range r {
			if j < len(keep) && !keep[j] {
				continue
			}
			nr = append(nr, v)
		}
		out[i] = nr
	}
	return out
}

func formatHTMLTable(table *html.Node) string {
	headerRows, dataRows := buildTableGrid(table)
	rows := make([][]string, 0, 1+len(dataRows))
	if header := mergeHeaderRows(headerRows); header != nil {
		rows = append(rows, header)
	}
	rows = append(rows, dataRows...)
	return renderASCIITable(dropUselessColumns(rows))
}

// buildTableGrid превращает <tr>/<td>/<th> с colspan/rowspan в выровненную по колонкам
// сетку. Таблицы умений на next.dnd.su (например, ячейки заклинаний по уровням у
// заклинателей) используют двухстрочный заголовок: верхняя строка с rowspan для общих
// колонок и colspan для сгруппированной подписи, нижняя — с номерами колонок группы.
// Без учёта span'ов колонки съезжают и таблица становится нечитаемой.
func buildTableGrid(table *html.Node) (headerRows, dataRows [][]string) {
	type pending struct {
		text   string
		remain int
	}
	carry := map[int]pending{}

	var trs []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			trs = append(trs, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)

	for _, tr := range trs {
		var cells []*html.Node
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data != "td" && c.Data != "th" {
				continue
			}
			cells = append(cells, c)
		}
		if len(cells) == 0 {
			continue
		}

		var row []string
		setCol := func(col int, v string) {
			for len(row) <= col {
				row = append(row, "")
			}
			row[col] = v
		}

		col, ci := 0, 0
		for ci < len(cells) || carry[col].remain > 0 {
			if p, ok := carry[col]; ok && p.remain > 0 {
				setCol(col, p.text)
				p.remain--
				if p.remain > 0 {
					carry[col] = p
				} else {
					delete(carry, col)
				}
				col++
				continue
			}
			if ci >= len(cells) {
				col++
				continue
			}
			cell := cells[ci]
			ci++
			colspan := attrInt(cell, "colspan", 1)
			rowspan := attrInt(cell, "rowspan", 1)
			text := cellText(cell)
			for k := 0; k < colspan; k++ {
				if k == 0 {
					setCol(col, text)
					if rowspan > 1 {
						carry[col] = pending{text: text, remain: rowspan - 1}
					}
				} else {
					setCol(col, "")
				}
				col++
			}
		}

		if hasClass(tr, "table_header") {
			headerRows = append(headerRows, row)
		} else {
			dataRows = append(dataRows, row)
		}
	}
	return headerRows, dataRows
}

// mergeHeaderRows схлопывает 1-2 строки заголовка в одну: если под колонкой есть
// значение в более поздней строке (например, номер круга заклинания под общей
// объединённой подписью), оно побеждает — так же, как выглядело бы на сайте после
// визуального объединения ячеек.
func mergeHeaderRows(headerRows [][]string) []string {
	if len(headerRows) == 0 {
		return nil
	}
	cols := 0
	for _, r := range headerRows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	out := make([]string, cols)
	for _, r := range headerRows {
		for j, v := range r {
			if strings.TrimSpace(v) != "" {
				out[j] = v
			}
		}
	}
	return out
}

func attrInt(n *html.Node, key string, def int) int {
	v := strings.TrimSpace(getAttr(n, key))
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil || i < 1 {
		return def
	}
	return i
}

func cellText(cell *html.Node) string {
	// предпочитаем полные подписи (.long), короткие уже вырезаны
	long := findElement(cell, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "long")
	})
	raw := ""
	if long != nil {
		raw = plainText(long)
	} else {
		raw = plainText(cell)
	}
	raw = strings.ReplaceAll(raw, "\n", " ")
	raw = strings.TrimSpace(reSpaces.ReplaceAllString(raw, " "))
	if raw == "" {
		raw = "—"
	}
	// ужимаем очень длинные ячейки умений, чтобы таблица влезала в Telegram
	if utf8.RuneCountInString(raw) > 42 {
		r := []rune(raw)
		raw = string(r[:39]) + "…"
	}
	return raw
}

func plainText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		if node.Type == html.ElementNode && node.Data == "br" {
			b.WriteString(" ")
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func renderASCIITable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	for cols > 1 {
		reallyEmpty := true
		for _, r := range rows {
			if len(r) >= cols {
				v := strings.TrimSpace(r[cols-1])
				if v != "" && v != "—" {
					reallyEmpty = false
					break
				}
			}
		}
		if !reallyEmpty {
			break
		}
		cols--
	}
	if cols < 1 {
		return ""
	}

	// выровнять число колонок во всех строках
	norm := make([][]string, len(rows))
	for i, r := range rows {
		row := make([]string, cols)
		for j := 0; j < cols; j++ {
			if j < len(r) && strings.TrimSpace(r[j]) != "" {
				row[j] = strings.TrimSpace(r[j])
			} else {
				row[j] = "-"
			}
		}
		norm[i] = row
	}
	rows = norm

	// короче заголовки для узкого экрана Telegram
	if len(rows) > 0 {
		for i, h := range rows[0] {
			rows[0][i] = shortenHeader(h)
		}
	}

	widths := make([]int, cols)
	for _, r := range rows {
		for i := 0; i < cols; i++ {
			w := utf8.RuneCountInString(r[i])
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	for i := range widths {
		if widths[i] < 1 {
			widths[i] = 1
		}
		if widths[i] > 22 {
			widths[i] = 22
		}
	}

	var b strings.Builder
	writeRow := func(r []string) {
		for i := 0; i < cols; i++ {
			if i > 0 {
				b.WriteString(" | ")
			}
			val := r[i]
			if utf8.RuneCountInString(val) > widths[i] {
				rr := []rune(val)
				val = string(rr[:widths[i]-1]) + "."
			}
			// числа и "-" — вправо, текст — влево
			if isNumericCell(val) {
				b.WriteString(padLeft(val, widths[i]))
			} else {
				b.WriteString(padRight(val, widths[i]))
			}
		}
		b.WriteByte('\n')
	}

	for ri, r := range rows {
		writeRow(r)
		if ri == 0 {
			for i := 0; i < cols; i++ {
				if i > 0 {
					b.WriteString("-+-")
				}
				b.WriteString(strings.Repeat("-", widths[i]))
			}
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func shortenHeader(h string) string {
	repl := []struct{ from, to string }{
		{"Уровень Воина", "Ур"},
		{"Уровень Паладина", "Ур"},
		{"Уровень", "Ур"},
		{"Бонус владения", "БВ"},
		{"Подготовленные заклинания", "Подг"},
		{"Подготовл. заклинания", "Подг"},
		{"Классовые умения", "Умения"},
		{"Проведение божественности", "ПБ"},
		{"Ячейки заклинаний на уровень заклинаний", "Ячейки"},
	}
	for _, r := range repl {
		if strings.EqualFold(h, r.from) || h == r.from {
			return r.to
		}
	}
	return h
}

func isNumericCell(s string) bool {
	if s == "-" || s == "—" || s == "~" || s == "+" {
		return true
	}
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func padLeft(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return strings.Repeat(" ", width-n) + s
}

func padRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

func hasClass(n *html.Node, want string) bool {
	for _, a := range n.Attr {
		if a.Key != "class" {
			continue
		}
		for _, c := range strings.Fields(a.Val) {
			if c == want {
				return true
			}
		}
	}
	return false
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
