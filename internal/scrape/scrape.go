package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/flash4612/dnd-telegram-bot/internal/store"
	"golang.org/x/net/html"
)

type CategorySpec struct {
	Category store.Category
	Path     string
	Mode     string // list_json | href_numbered | href_slug
}

var Categories = []CategorySpec{
	{Category: store.CategoryClass, Path: "/class/", Mode: "href_slug"},
	{Category: store.CategorySpecies, Path: "/species/", Mode: "href_slug"},
	{Category: store.CategoryBackground, Path: "/backgrounds/", Mode: "href_numbered"},
	{Category: store.CategorySpell, Path: "/spells/", Mode: "list_json"},
	{Category: store.CategoryItem, Path: "/items/", Mode: "list_json"},
	{Category: store.CategoryEquipment, Path: "/equipment/", Mode: "href_numbered"},
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Delay      time.Duration
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
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 45 * time.Second,
		},
		Delay: 350 * time.Millisecond,
	}
}

func (c *Client) IngestAll(ctx context.Context, st *store.Store, progress ProgressFunc) error {
	for _, cat := range Categories {
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
	body, err := c.get(ctx, cat.Path)
	if err != nil {
		return nil, err
	}

	switch cat.Mode {
	case "list_json":
		return parseWindowList(body)
	case "href_numbered":
		re := regexp.MustCompile(`href="(` + regexp.QuoteMeta(strings.TrimSuffix(cat.Path, "/")) + `/\d+-[a-zA-Z0-9\-]+)/?"`)
		return uniqueLinks(re.FindAllStringSubmatch(body, -1)), nil
	case "href_slug":
		re := regexp.MustCompile(`href="(` + regexp.QuoteMeta(strings.TrimSuffix(cat.Path, "/")) + `/[a-z][a-zA-Z0-9\-]+)/?"`)
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

	titleRU, titleEN, sourceMeta, textBody, err := parseCard(body)
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

	summary := firstParagraph(textBody)
	return store.Entry{
		Category:   category,
		Slug:       slug,
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

func parseCard(pageHTML string) (titleRU, titleEN, sourceMeta, body string, err error) {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return "", "", "", "", err
	}

	card := findElement(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "div" &&
			hasClass(n, "card") && hasClass(n, "paper-1")
	})
	if card == nil {
		card = findElement(doc, func(n *html.Node) bool {
			return n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "card") && hasClass(n, "typoable")
		})
	}
	if card == nil {
		return "", "", "", "", fmt.Errorf("card not found")
	}

	titleNode := findElement(card, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "card-title")
	})
	rawTitle := strings.TrimSpace(textContent(titleNode))
	rawTitle = strings.ReplaceAll(rawTitle, "Распечатать", "")
	rawTitle = strings.TrimSpace(rawTitle)
	titleRU, titleEN = splitTitle(rawTitle)

	sourceMeta = collectSources(titleNode)

	bodyNode := findElement(card, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "card__body")
	})
	body = normalizeText(textContent(bodyNode))
	if body == "" {
		return "", "", "", "", fmt.Errorf("empty body")
	}
	return titleRU, titleEN, sourceMeta, body, nil
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
	return strings.TrimSpace(strings.Join(out, "\n"))
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
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		if node.Type == html.ElementNode {
			switch node.Data {
			case "br":
				b.WriteString("\n")
			case "p", "li", "h1", "h2", "h3", "h4", "tr":
				if b.Len() > 0 {
					b.WriteString("\n")
				}
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if node.Type == html.ElementNode {
			switch node.Data {
			case "p", "li", "tr":
				b.WriteString("\n")
			}
		}
	}
	walk(n)
	return b.String()
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
