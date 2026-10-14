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
	CategoryFeat       Category = "feat"
	CategoryFormulas   Category = "formulas"
	CategoryMechanics  Category = "mechanics"
)

var CategoryLabels = map[Category]string{
	CategoryClass:      "Класс",
	CategorySpecies:    "Вид",
	CategoryBackground: "Предыстория",
	CategorySpell:      "Заклинание",
	CategoryItem:       "Магический предмет",
	CategoryEquipment:  "Снаряжение",
	CategoryFeat:       "Черта",
	CategoryFormulas:   "Основные формулы",
	CategoryMechanics:  "Игровая механика",
}

// Edition — редакция правил: "2024" (next.dnd.su, основная) или "2014" (dnd.su, классическая).
const (
	Edition2024 = "2024"
	Edition2014 = "2014"
)

type Entry struct {
	ID         int64
	Category   Category
	Slug       string
	Edition    string
	URL        string
	TitleRU    string
	TitleEN    string
	Summary    string
	Body       string
	SourceMeta string
	UpdatedAt  time.Time
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
  edition TEXT NOT NULL DEFAULT '2024',
  url TEXT NOT NULL,
  title_ru TEXT NOT NULL,
  title_en TEXT NOT NULL DEFAULT '',
  title_ru_lc TEXT NOT NULL DEFAULT '',
  title_en_lc TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  source_meta TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  UNIQUE(category, slug, edition)
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
	if err != nil {
		return err
	}
	// Старые БД без колонок lc.
	_, _ = s.db.Exec(`ALTER TABLE entries ADD COLUMN title_ru_lc TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE entries ADD COLUMN title_en_lc TEXT NOT NULL DEFAULT ''`)
	if err := s.migrateEditionColumn(); err != nil {
		return err
	}
	return s.backfillTitleLC()
}

// migrateEditionColumn переносит старые базы (до появления поддержки редакции 2014) на
// схему с UNIQUE(category, slug, edition). Простой ALTER TABLE ADD COLUMN тут не годится:
// старый уникальный индекс UNIQUE(category, slug) не даст сохранить запись 2014-редакции
// с тем же slug, что и у уже существующей 2024-записи — поэтому таблицу пересоздаём.
func (s *Store) migrateEditionColumn() error {
	hasEdition := false
	rows, err := s.db.Query(`PRAGMA table_info(entries)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			cid                                          int
			name, colType                                string
			notNull, pk                                  int
			dflt                                          sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "edition" {
			hasEdition = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if hasEdition {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
ALTER TABLE entries RENAME TO entries_old;

CREATE TABLE entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  category TEXT NOT NULL,
  slug TEXT NOT NULL,
  edition TEXT NOT NULL DEFAULT '2024',
  url TEXT NOT NULL,
  title_ru TEXT NOT NULL,
  title_en TEXT NOT NULL DEFAULT '',
  title_ru_lc TEXT NOT NULL DEFAULT '',
  title_en_lc TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  source_meta TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  UNIQUE(category, slug, edition)
);

INSERT INTO entries (id, category, slug, edition, url, title_ru, title_en, title_ru_lc, title_en_lc, summary, body, source_meta, updated_at)
SELECT id, category, slug, '2024', url, title_ru, title_en, title_ru_lc, title_en_lc, summary, body, source_meta, updated_at
FROM entries_old;

DROP TABLE entries_old;
`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// entries_fts — external content table поверх entries; после пересоздания таблицы
	// перестраиваем индекс на всякий случай, чтобы не полагаться на совпадение rowid.
	_, err = s.db.Exec(`INSERT INTO entries_fts(entries_fts) VALUES ('rebuild')`)
	return err
}

func (s *Store) backfillTitleLC() error {
	rows, err := s.db.Query(`
SELECT id, title_ru, title_en FROM entries
WHERE title_ru_lc = '' OR (title_en != '' AND title_en_lc = '')
`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		id            int64
		titleRU, titleEN string
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.titleRU, &r.titleEN); err != nil {
			return err
		}
		todo = append(todo, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE entries SET title_ru_lc = ?, title_en_lc = ? WHERE id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range todo {
		if _, err := stmt.Exec(strings.ToLower(r.titleRU), strings.ToLower(r.titleEN), r.id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Upsert(ctx context.Context, e Entry) error {
	now := time.Now().UTC().Format(time.RFC3339)
	ruLC := strings.ToLower(e.TitleRU)
	enLC := strings.ToLower(e.TitleEN)
	edition := e.Edition
	if edition == "" {
		edition = Edition2024
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO entries (category, slug, edition, url, title_ru, title_en, title_ru_lc, title_en_lc, summary, body, source_meta, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(category, slug, edition) DO UPDATE SET
  url=excluded.url,
  title_ru=excluded.title_ru,
  title_en=excluded.title_en,
  title_ru_lc=excluded.title_ru_lc,
  title_en_lc=excluded.title_en_lc,
  summary=excluded.summary,
  body=excluded.body,
  source_meta=excluded.source_meta,
  updated_at=excluded.updated_at
`, e.Category, e.Slug, edition, e.URL, e.TitleRU, e.TitleEN, ruLC, enLC, e.Summary, e.Body, e.SourceMeta, now)
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

// TitleEntry — облегчённая запись для построения меню (без Body).
type TitleEntry struct {
	ID      int64
	TitleRU string
	TitleEN string
}

func (s *Store) ListTitles(ctx context.Context, category Category, edition string) ([]TitleEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, title_ru, title_en FROM entries
WHERE category = ? AND edition = ?
ORDER BY title_ru`, category, edition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleEntry
	for rows.Next() {
		var t TitleEntry
		if err := rows.Scan(&t.ID, &t.TitleRU, &t.TitleEN); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetByID(ctx context.Context, id int64) (Entry, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, category, slug, edition, url, title_ru, title_en, summary, body, source_meta, updated_at
FROM entries WHERE id = ?`, id)

	var e Entry
	var updated string
	if err := row.Scan(&e.ID, &e.Category, &e.Slug, &e.Edition, &e.URL, &e.TitleRU, &e.TitleEN, &e.Summary, &e.Body, &e.SourceMeta, &updated); err != nil {
		return Entry{}, err
	}
	if t, err := time.Parse(time.RFC3339, updated); err == nil {
		e.UpdatedAt = t
	}
	return e, nil
}

// Search ищет только в основной редакции (2024) — это то, что видит пользователь по
// умолчанию. Для явного поиска в другой редакции используй SearchEdition.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return s.SearchEdition(ctx, query, Edition2024, limit)
}

func (s *Store) SearchEdition(ctx context.Context, query, edition string, limit int) ([]SearchHit, error) {
	query = normalizeQuery(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}

	if hits, err := s.searchTitles(ctx, query, edition, limit); err == nil && len(hits) > 0 {
		return hits, nil
	}

	// Тело — только запасной вариант, слабые совпадения.
	return s.searchBodyFallback(ctx, query, limit)
}

// FindByTitle ищет точный аналог записи в другой редакции того же раздела — сперва по
// английскому названию (оно обычно стабильнее между редакциями), затем по русскому.
func (s *Store) FindByTitle(ctx context.Context, category Category, edition, titleRU, titleEN string) (Entry, bool, error) {
	ruLC := strings.ToLower(strings.TrimSpace(titleRU))
	enLC := strings.ToLower(strings.TrimSpace(titleEN))
	if ruLC == "" && enLC == "" {
		return Entry{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, `
SELECT id, category, slug, edition, url, title_ru, title_en, summary, body, source_meta, updated_at
FROM entries
WHERE category = ? AND edition = ? AND ((? != '' AND title_en_lc = ?) OR (? != '' AND title_ru_lc = ?))
ORDER BY (? != '' AND title_en_lc = ?) DESC
LIMIT 1
`, category, edition, enLC, enLC, ruLC, ruLC, enLC, enLC)

	var e Entry
	var updated string
	if err := row.Scan(&e.ID, &e.Category, &e.Slug, &e.Edition, &e.URL, &e.TitleRU, &e.TitleEN, &e.Summary, &e.Body, &e.SourceMeta, &updated); err != nil {
		if err == sql.ErrNoRows {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	if t, err := time.Parse(time.RFC3339, updated); err == nil {
		e.UpdatedAt = t
	}
	return e, true, nil
}

func normalizeQuery(q string) string {
	q = strings.TrimSpace(q)
	q = strings.ToLower(q)
	prefixes := []string{
		"заклинание ", "спелл ", "spell ",
		"предмет ", "магический предмет ", "артефакт ",
		"класс ", "вид ", "раса ", "предыстория ", "снаряжение ",
		"feat ", "черта ",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(q, p) {
			q = strings.TrimSpace(q[len(p):])
			break
		}
	}
	return strings.Join(strings.Fields(q), " ")
}

func (s *Store) searchTitles(ctx context.Context, query, edition string, limit int) ([]SearchHit, error) {
	like := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx, `
SELECT id, category, slug, edition, url, title_ru, title_en, summary, body, source_meta, updated_at,
  CASE
    WHEN title_ru_lc = ? OR title_en_lc = ? THEN 0
    WHEN title_ru_lc LIKE ? OR title_en_lc LIKE ? THEN 1
    WHEN title_ru_lc LIKE ? OR title_en_lc LIKE ? THEN 2
    ELSE 3
  END AS rank
FROM entries
WHERE edition = ? AND (
   title_ru_lc = ? OR title_en_lc = ?
   OR title_ru_lc LIKE ? OR title_en_lc LIKE ?
   OR title_ru_lc LIKE ? OR title_en_lc LIKE ?
)
ORDER BY rank, length(title_ru)
LIMIT ?
`,
		query, query,
		query+"%", query+"%",
		like, like,
		edition,
		query, query,
		query+"%", query+"%",
		like, like,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHits(rows)
}

func (s *Store) searchBodyFallback(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	// Без автоответа по «упоминанию в тексте»: только если очень нужно показать варианты.
	// Пока отключаем body-поиск, чтобы «Мастер оружия» не находило копьё по «Мастерство».
	_ = ctx
	_ = query
	_ = limit
	return nil, nil
}

func scanHits(rows *sql.Rows) ([]SearchHit, error) {
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var updated string
		var rank float64
		if err := rows.Scan(
			&h.ID, &h.Category, &h.Slug, &h.Edition, &h.URL, &h.TitleRU, &h.TitleEN,
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
