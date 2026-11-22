package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The reference uses Kotlin Float, whose JSON representation includes a decimal
// point even for whole numbers. Keep both its precision and wire representation.
type wireFloat float32

func (f wireFloat) MarshalJSON() ([]byte, error) {
	s := strconv.FormatFloat(float64(f), 'g', -1, 32)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return []byte(s), nil
}

type MangaTag struct {
	ID     int64  `json:"tag_id"`
	Title  string `json:"title"`
	Key    string `json:"key"`
	Source string `json:"source"`
	Pinned bool   `json:"pinned"`
}

type Manga struct {
	ID            int64      `json:"manga_id"`
	Title         string     `json:"title"`
	AltTitle      *string    `json:"alt_title"`
	URL           string     `json:"url"`
	PublicURL     string     `json:"public_url"`
	Rating        wireFloat  `json:"rating"`
	ContentRating *string    `json:"content_rating"`
	CoverURL      string     `json:"cover_url"`
	LargeCoverURL *string    `json:"large_cover_url"`
	Tags          []MangaTag `json:"tags"`
	State         *string    `json:"state"`
	Author        *string    `json:"author"`
	Source        string     `json:"source"`
	NSFW          bool       `json:"nsfw"`
}

type dbQuerier interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func objectFields(raw json.RawMessage, required, nullable []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("expected an object")
	}
	for _, name := range required {
		value, ok := fields[name]
		if !ok || string(value) == "null" {
			return nil, fmt.Errorf("missing or null %s", name)
		}
	}
	for _, name := range nullable {
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("missing %s", name)
		}
	}
	return fields, nil
}

func validateManga(raw json.RawMessage) error {
	fields, err := objectFields(raw, []string{"manga_id", "title", "url", "public_url", "rating", "cover_url", "tags", "source"}, []string{"alt_title", "large_cover_url", "state", "author"})
	if err != nil {
		return err
	}
	if value, ok := fields["nsfw"]; ok && string(value) == "null" {
		return errors.New("null nsfw")
	}
	var tags []json.RawMessage
	if err := json.Unmarshal(fields["tags"], &tags); err != nil || tags == nil {
		return errors.New("invalid tags")
	}
	for _, tag := range tags {
		tagFields, err := objectFields(tag, []string{"tag_id", "title", "key", "source"}, nil)
		if err != nil {
			return err
		}
		if value, ok := tagFields["pinned"]; ok && string(value) == "null" {
			return errors.New("null pinned")
		}
	}
	return nil
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}

func truncatePointer(s *string, n int) *string {
	if s == nil {
		return nil
	}
	v := truncate(*s, n)
	return &v
}

func enumValue(s *string, allowed ...string) *string {
	if s == nil {
		return nil
	}
	for _, value := range allowed {
		if *s == value {
			return s
		}
	}
	return nil
}

func normalizedManga(m Manga) Manga {
	m.Title = truncate(m.Title, 100)
	m.AltTitle = truncatePointer(m.AltTitle, 100)
	m.URL = truncate(m.URL, 255)
	m.PublicURL = truncate(m.PublicURL, 255)
	m.CoverURL = truncate(m.CoverURL, 255)
	m.LargeCoverURL = truncatePointer(m.LargeCoverURL, 255)
	if m.LargeCoverURL == nil {
		empty := ""
		m.LargeCoverURL = &empty
	}
	m.Author = truncatePointer(m.Author, 64)
	m.Source = truncate(m.Source, 32)
	m.ContentRating = enumValue(m.ContentRating, "SAFE", "SUGGESTIVE", "ADULT")
	m.State = enumValue(m.State, "ONGOING", "FINISHED", "ABANDONED", "PAUSED", "UPCOMING")
	return m
}

func (a *app) initManga() error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS catalog_manga (id BIGINT PRIMARY KEY, payload LONGTEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS catalog_tags (id BIGINT PRIMARY KEY, payload LONGTEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS catalog_manga_tags (manga_id BIGINT NOT NULL, tag_id BIGINT NOT NULL, PRIMARY KEY (manga_id, tag_id))`,
	} {
		if _, err := a.db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) saveManga(tx *sql.Tx, m Manga) error {
	m = normalizedManga(m)
	tags := m.Tags
	m.Tags = nil
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	mangaSQL := `INSERT INTO catalog_manga (id, payload) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET payload = excluded.payload`
	tagSQL := `INSERT INTO catalog_tags (id, payload) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET payload = excluded.payload`
	linkSQL := `INSERT OR IGNORE INTO catalog_manga_tags (manga_id, tag_id) VALUES (?, ?)`
	if a.dialect == "mysql" {
		mangaSQL = `INSERT INTO catalog_manga (id, payload) VALUES (?, ?) ON DUPLICATE KEY UPDATE payload = VALUES(payload)`
		tagSQL = `INSERT INTO catalog_tags (id, payload) VALUES (?, ?) ON DUPLICATE KEY UPDATE payload = VALUES(payload)`
		linkSQL = `INSERT IGNORE INTO catalog_manga_tags (manga_id, tag_id) VALUES (?, ?)`
	}
	if _, err = tx.Exec(mangaSQL, m.ID, string(payload)); err != nil {
		return err
	}
	for _, tag := range tags {
		tag.Title = truncate(tag.Title, 64)
		tag.Key = truncate(tag.Key, 120)
		tag.Source = truncate(tag.Source, 32)
		data, err := json.Marshal(tag)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(tagSQL, tag.ID, string(data)); err != nil {
			return err
		}
		if _, err = tx.Exec(linkSQL, m.ID, tag.ID); err != nil {
			return err
		}
	}
	return nil
}

// hydrateManga loads tags in bounded batches, after closing the outer result
// set, so it also works with a single SQLite connection.
func hydrateManga(q dbQuerier, manga []*Manga) error {
	if len(manga) == 0 {
		return nil
	}
	byID := make(map[int64][]*Manga, len(manga))
	ids := make([]int64, 0, len(manga))
	for _, m := range manga {
		m.Tags = make([]MangaTag, 0)
		if _, exists := byID[m.ID]; !exists {
			ids = append(ids, m.ID)
		}
		byID[m.ID] = append(byID[m.ID], m)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", end-start), ",")
		args := make([]any, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		rows, err := q.Query(`SELECT mt.manga_id, t.payload FROM catalog_manga_tags mt JOIN catalog_tags t ON t.id = mt.tag_id WHERE mt.manga_id IN (`+placeholders+`) ORDER BY mt.manga_id, mt.tag_id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var payload string
			if err := rows.Scan(&id, &payload); err != nil {
				rows.Close()
				return err
			}
			var tag MangaTag
			if err := json.Unmarshal([]byte(payload), &tag); err != nil {
				rows.Close()
				return err
			}
			for _, m := range byID[id] {
				m.Tags = append(m.Tags, tag)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func readManga(q dbQuerier, id int64) (Manga, error) {
	var m Manga
	var payload string
	if err := q.QueryRow(`SELECT payload FROM catalog_manga WHERE id = ?`, id).Scan(&payload); err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return m, err
	}
	if err := hydrateManga(q, []*Manga{&m}); err != nil {
		return m, err
	}
	return m, nil
}

func (a *app) registerManga(mux *http.ServeMux) {
	mux.HandleFunc("GET /manga", func(w http.ResponseWriter, r *http.Request) {
		offset, err1 := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, err2 := strconv.Atoi(r.URL.Query().Get("limit"))
		if err1 != nil || err2 != nil || offset < 0 || limit < 0 {
			respondError(w, http.StatusBadRequest, "Invalid offset or limit")
			return
		}
		rows, err := a.db.Query(`SELECT payload FROM catalog_manga ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Database error")
			return
		}
		result := make([]Manga, 0)
		for rows.Next() {
			var payload string
			var m Manga
			if err = rows.Scan(&payload); err != nil {
				break
			}
			if err = json.Unmarshal([]byte(payload), &m); err != nil {
				break
			}
			result = append(result, m)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Database error")
			return
		}
		pointers := make([]*Manga, len(result))
		for i := range result {
			pointers[i] = &result[i]
		}
		if err := hydrateManga(a.db, pointers); err != nil {
			respondError(w, http.StatusInternalServerError, "Database error")
			return
		}
		respondJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /manga/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			respondError(w, http.StatusNotFound, "Not Found")
			return
		}
		manga, err := readManga(a.db, id)
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "Not Found")
			return
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Database error")
			return
		}
		respondJSON(w, http.StatusOK, manga)
	})
}
