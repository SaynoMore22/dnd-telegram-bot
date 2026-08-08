package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Category string

const (
	CategoryClass      Category = "class"
	CategorySpecies    Category = "species"
	CategoryBackground Category = "background"
	CategorySpell      Category = "spell"
	CategoryItem       Category = "item"
	CategoryEquipment  Category = "equipment"
)

var CategoryLabels = map[Category]string{
	CategoryClass:      "Класс",
	CategorySpecies:    "Вид",
	CategoryBackground: "Предыстория",
	CategorySpell:      "Заклинание",
	CategoryItem:       "Магический предмет",
	CategoryEquipment:  "Снаряжение",
}

type Entry struct {
	ID          int64
	Category    Category
	Slug        string
	URL         string
	TitleRU     string
	TitleEN     string
	Summary     string
	Body        string
	SourceMeta  string
	UpdatedAt   time.Time
}

type SearchHit struct {
	Entry
	Score float64
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  category TEXT NOT NULL,
  slug TEXT NOT NULL,
  url TEXT NOT NULL,
  title_ru TEXT NOT NULL,
  title_en TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  source_meta TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  UNIQUE(category, slug)
);

CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
  title_ru,
  title_en,
  summary,
  body,
  content='entries',
  content_rowid='id',
  tokenize='unicode61'
);

CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
  INSERT INTO entries_fts(rowid, title_ru, title_en, summary, body)
  VALUES (new.id, new.title_ru, new.title_en, new.summary, new.body);
END;

CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
  INSERT INTO entries_fts(entries_fts, rowid, title_ru, title_en, summary, body)
  VALUES ('delete', old.id, old.title_ru, old.title_en, old.summary, old.body);
END;

CREATE TRIGGER IF NOT EXISTS entries_au AFTER UPDATE ON entries BEGIN
  INSERT INTO entries_fts(entries_fts, rowid, title_ru, title_en, summary, body)
  VALUES ('delete', old.id, old.title_ru, old.title_en, old.summary, old.body);
  INSERT INTO entries_fts(rowid, title_ru, title_en, summary, body)
  VALUES (new.id, new.title_ru, new.title_en, new.summary, new.body);
END;
`)
	return err
}

func (s *Store) Upsert(ctx context.Context, e Entry) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO entries (category, slug, url, title_ru, title_en, summary, body, source_meta, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(category, slug) DO UPDATE SET
  url=excluded.url,
  title_ru=excluded.title_ru,
  title_en=excluded.title_en,
  summary=excluded.summary,
  body=excluded.body,
  source_meta=excluded.source_meta,
  updated_at=excluded.updated_at
`, e.Category, e.Slug, e.URL, e.TitleRU, e.TitleEN, e.Summary, e.Body, e.SourceMeta, now)
	return err
}

func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries`).Scan(&n)
	return n, err
}

func (s *Store) CountByCategory(ctx context.Context) (map[Category]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT category, COUNT(*) FROM entries GROUP BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[Category]int)
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		out[Category(c)] = n
	}
	return out, rows.Err()
}

func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}

	ftsQuery := buildFTSQuery(query)
	hits, err := s.searchFTS(ctx, ftsQuery, limit)
	if err == nil && len(hits) > 0 {
		return hits, nil
	}

	return s.searchLike(ctx, query, limit)
}

func (s *Store) searchFTS(ctx context.Context, ftsQuery string, limit int) ([]SearchHit, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT e.id, e.category, e.slug, e.url, e.title_ru, e.title_en, e.summary, e.body, e.source_meta, e.updated_at,
       bm25(entries_fts) AS rank
FROM entries_fts
JOIN entries e ON e.id = entries_fts.rowid
WHERE entries_fts MATCH ?
ORDER BY rank
LIMIT ?
`, ftsQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHits(rows)
}

func (s *Store) searchLike(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	like := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.QueryContext(ctx, `
SELECT id, category, slug, url, title_ru, title_en, summary, body, source_meta, updated_at, 0 AS rank
FROM entries
WHERE lower(title_ru) LIKE ? OR lower(title_en) LIKE ? OR lower(summary) LIKE ?
ORDER BY
  CASE
    WHEN lower(title_ru) = lower(?) OR lower(title_en) = lower(?) THEN 0
    WHEN lower(title_ru) LIKE ? OR lower(title_en) LIKE ? THEN 1
    ELSE 2
  END,
  length(title_ru)
LIMIT ?
`, like, like, like, query, query, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHits(rows)
}

func scanHits(rows *sql.Rows) ([]SearchHit, error) {
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var updated string
		var rank float64
		if err := rows.Scan(
			&h.ID, &h.Category, &h.Slug, &h.URL, &h.TitleRU, &h.TitleEN,
			&h.Summary, &h.Body, &h.SourceMeta, &updated, &rank,
		); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, updated); err == nil {
			h.UpdatedAt = t
		}
		h.Score = rank
		out = append(out, h)
	}
	return out, rows.Err()
}

func buildFTSQuery(q string) string {
	parts := strings.Fields(q)
	var terms []string
	for _, p := range parts {
		p = strings.Trim(p, `"*`)
		if p == "" {
			continue
		}
		p = strings.ReplaceAll(p, `"`, "")
		terms = append(terms, `"`+p+`"*`)
	}
	if len(terms) == 0 {
		return `"` + strings.ReplaceAll(q, `"`, "") + `"*`
	}
	return strings.Join(terms, " ")
}
