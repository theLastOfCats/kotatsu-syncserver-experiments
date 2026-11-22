package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

type History struct {
	MangaID   int64     `json:"manga_id"`
	Manga     Manga     `json:"manga"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
	ChapterID int64     `json:"chapter_id"`
	Page      int       `json:"page"`
	Scroll    wireFloat `json:"scroll"`
	Percent   wireFloat `json:"percent"`
	Chapters  int       `json:"chapters"`
	DeletedAt int64     `json:"deleted_at"`
}

type Category struct {
	ID        int64  `json:"category_id"`
	CreatedAt int64  `json:"created_at"`
	SortKey   int    `json:"sort_key"`
	Track     bool   `json:"track"`
	Title     string `json:"title"`
	Order     string `json:"order"`
	DeletedAt int64  `json:"deleted_at"`
	ShowInLib bool   `json:"show_in_lib"`
}

type Favourite struct {
	MangaID    int64 `json:"manga_id"`
	Manga      Manga `json:"manga"`
	CategoryID int64 `json:"category_id"`
	SortKey    int   `json:"sort_key"`
	Pinned     bool  `json:"pinned"`
	CreatedAt  int64 `json:"created_at"`
	DeletedAt  int64 `json:"deleted_at"`
}

type HistoryPackage struct {
	History   []History `json:"history"`
	Timestamp int64     `json:"timestamp"`
}

type FavouritesPackage struct {
	Categories []Category  `json:"categories"`
	Favourites []Favourite `json:"favourites"`
	Timestamp  int64       `json:"timestamp"`
}

func (a *app) initSync() error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS sync_state (user_id BIGINT PRIMARY KEY, history_at BIGINT NOT NULL, favourites_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sync_history (user_id BIGINT NOT NULL, manga_id BIGINT NOT NULL, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL, chapter_id BIGINT NOT NULL, page INTEGER NOT NULL, scroll FLOAT NOT NULL, percent FLOAT NOT NULL, chapters INTEGER NOT NULL, deleted_at BIGINT NOT NULL, PRIMARY KEY (user_id, manga_id))`,
		`CREATE TABLE IF NOT EXISTS sync_categories (user_id BIGINT NOT NULL, category_id BIGINT NOT NULL, created_at BIGINT NOT NULL, sort_key INTEGER NOT NULL, track INTEGER NOT NULL, title VARCHAR(120) NOT NULL, sort_order VARCHAR(255) NOT NULL, deleted_at BIGINT NOT NULL, show_in_lib INTEGER NOT NULL, PRIMARY KEY (user_id, category_id))`,
		`CREATE TABLE IF NOT EXISTS sync_favourites (user_id BIGINT NOT NULL, manga_id BIGINT NOT NULL, category_id BIGINT NOT NULL, sort_key INTEGER NOT NULL, pinned INTEGER NOT NULL, created_at BIGINT NOT NULL, deleted_at BIGINT NOT NULL, PRIMARY KEY (user_id, manga_id, category_id))`,
	} {
		if _, err := a.db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

func parseBody(r *http.Request) (map[string]json.RawMessage, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<20 {
		return nil, errors.New("request too large")
	}
	return objectFields(data, nil, []string{"timestamp"})
}

func parseRows(fields map[string]json.RawMessage, key string) ([]json.RawMessage, error) {
	data, ok := fields[key]
	if !ok || string(data) == "null" {
		return nil, fmt.Errorf("missing %s", key)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil || rows == nil {
		return nil, fmt.Errorf("invalid %s", key)
	}
	var timestamp *int64
	if err := json.Unmarshal(fields["timestamp"], &timestamp); err != nil {
		return nil, errors.New("invalid timestamp")
	}
	return rows, nil
}

func decodeHistory(r *http.Request) ([]History, error) {
	fields, err := parseBody(r)
	if err != nil {
		return nil, err
	}
	rows, err := parseRows(fields, "history")
	if err != nil {
		return nil, err
	}
	result := make([]History, 0, len(rows))
	for _, raw := range rows {
		fields, err := objectFields(raw, []string{"manga_id", "manga", "created_at", "updated_at", "chapter_id", "page", "scroll", "percent", "deleted_at"}, nil)
		if err != nil {
			return nil, err
		}
		if value, ok := fields["chapters"]; ok && string(value) == "null" {
			return nil, errors.New("null chapters")
		}
		if err = validateManga(fields["manga"]); err != nil {
			return nil, err
		}
		row := History{Chapters: -1}
		if err = json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		if row.MangaID != row.Manga.ID {
			return nil, errors.New("manga id mismatch")
		}
		result = append(result, row)
	}
	return result, nil
}

func decodeFavourites(r *http.Request) ([]Category, []Favourite, error) {
	fields, err := parseBody(r)
	if err != nil {
		return nil, nil, err
	}
	categoryRows, err := parseRows(fields, "categories")
	if err != nil {
		return nil, nil, err
	}
	favouriteRows, err := parseRows(fields, "favourites")
	if err != nil {
		return nil, nil, err
	}
	categories := make([]Category, 0, len(categoryRows))
	for _, raw := range categoryRows {
		if _, err := objectFields(raw, []string{"category_id", "created_at", "sort_key", "track", "title", "order", "deleted_at", "show_in_lib"}, nil); err != nil {
			return nil, nil, err
		}
		var row Category
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, nil, err
		}
		categories = append(categories, row)
	}
	favourites := make([]Favourite, 0, len(favouriteRows))
	for _, raw := range favouriteRows {
		fields, err := objectFields(raw, []string{"manga_id", "manga", "category_id", "sort_key", "pinned", "created_at", "deleted_at"}, nil)
		if err != nil {
			return nil, nil, err
		}
		if err = validateManga(fields["manga"]); err != nil {
			return nil, nil, err
		}
		var row Favourite
		if err = json.Unmarshal(raw, &row); err != nil {
			return nil, nil, err
		}
		if row.MangaID != row.Manga.ID {
			return nil, nil, errors.New("manga id mismatch")
		}
		favourites = append(favourites, row)
	}
	return categories, favourites, nil
}

func syncTimestamp(q dbQuerier, user int64, column string) (int64, error) {
	// column is selected only by our handlers, never from user input.
	var timestamp int64
	err := q.QueryRow(`SELECT `+column+` FROM sync_state WHERE user_id = ?`, user).Scan(&timestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return timestamp, err
}

func (a *app) setTimestamp(tx *sql.Tx, user int64, history bool) error {
	now := time.Now().UnixMilli()
	var query string
	if history {
		query = `INSERT INTO sync_state (user_id, history_at, favourites_at) VALUES (?, ?, 0) ON CONFLICT(user_id) DO UPDATE SET history_at = excluded.history_at`
		if a.dialect == "mysql" {
			query = `INSERT INTO sync_state (user_id, history_at, favourites_at) VALUES (?, ?, 0) ON DUPLICATE KEY UPDATE history_at = VALUES(history_at)`
		}
	} else {
		query = `INSERT INTO sync_state (user_id, history_at, favourites_at) VALUES (?, 0, ?) ON CONFLICT(user_id) DO UPDATE SET favourites_at = excluded.favourites_at`
		if a.dialect == "mysql" {
			query = `INSERT INTO sync_state (user_id, history_at, favourites_at) VALUES (?, 0, ?) ON DUPLICATE KEY UPDATE favourites_at = VALUES(favourites_at)`
		}
	}
	_, err := tx.Exec(query, user, now)
	return err
}

func readHistory(q dbQuerier, user int64) (HistoryPackage, error) {
	result := HistoryPackage{History: make([]History, 0)}
	var err error
	result.Timestamp, err = syncTimestamp(q, user, "history_at")
	if err != nil {
		return result, err
	}
	rows, err := q.Query(`SELECT h.manga_id, m.payload, h.created_at, h.updated_at, h.chapter_id, h.page, h.scroll, h.percent, h.chapters, h.deleted_at FROM sync_history h JOIN catalog_manga m ON m.id = h.manga_id WHERE h.user_id = ? ORDER BY h.manga_id`, user)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var h History
		var data string
		var scroll, percent float64
		if err = rows.Scan(&h.MangaID, &data, &h.CreatedAt, &h.UpdatedAt, &h.ChapterID, &h.Page, &scroll, &percent, &h.Chapters, &h.DeletedAt); err != nil {
			break
		}
		h.Scroll, h.Percent = wireFloat(scroll), wireFloat(percent)
		if err = json.Unmarshal([]byte(data), &h.Manga); err != nil {
			break
		}
		result.History = append(result.History, h)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	cards := make([]*Manga, len(result.History))
	for i := range result.History {
		cards[i] = &result.History[i].Manga
	}
	return result, hydrateManga(q, cards)
}

func readFavourites(q dbQuerier, user int64) (FavouritesPackage, error) {
	result := FavouritesPackage{Categories: make([]Category, 0), Favourites: make([]Favourite, 0)}
	var err error
	result.Timestamp, err = syncTimestamp(q, user, "favourites_at")
	if err != nil {
		return result, err
	}
	rows, err := q.Query(`SELECT category_id, created_at, sort_key, track, title, sort_order, deleted_at, show_in_lib FROM sync_categories WHERE user_id = ? ORDER BY category_id`, user)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var c Category
		if err = rows.Scan(&c.ID, &c.CreatedAt, &c.SortKey, &c.Track, &c.Title, &c.Order, &c.DeletedAt, &c.ShowInLib); err != nil {
			break
		}
		result.Categories = append(result.Categories, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = q.Query(`SELECT f.manga_id, m.payload, f.category_id, f.sort_key, f.pinned, f.created_at, f.deleted_at FROM sync_favourites f JOIN catalog_manga m ON m.id = f.manga_id WHERE f.user_id = ? ORDER BY f.manga_id, f.category_id`, user)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var f Favourite
		var data string
		if err = rows.Scan(&f.MangaID, &data, &f.CategoryID, &f.SortKey, &f.Pinned, &f.CreatedAt, &f.DeletedAt); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(data), &f.Manga); err != nil {
			break
		}
		result.Favourites = append(result.Favourites, f)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	cards := make([]*Manga, len(result.Favourites))
	for i := range result.Favourites {
		cards[i] = &result.Favourites[i].Manga
	}
	return result, hydrateManga(q, cards)
}

func (a *app) lockUser(tx *sql.Tx, user int64) error {
	if a.dialect != "mysql" {
		return nil
	}
	var id int64
	return tx.QueryRow(`SELECT id FROM users WHERE id = ? FOR UPDATE`, user).Scan(&id)
}

// at is updated_at for history and created_at for categories and favourites.
type syncClock struct {
	at      int64
	deleted int64
	exists  bool
}

type favouriteKey struct {
	manga    int64
	category int64
}

func readClockBatch(tx *sql.Tx, prefix string, args []any, clocks map[int64]syncClock) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)-1), ",")
	rows, err := tx.Query(prefix+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var clock syncClock
		if err := rows.Scan(&id, &clock.at, &clock.deleted); err != nil {
			return err
		}
		clock.exists = true
		clocks[id] = clock
	}
	return rows.Err()
}

// The map is also the candidate set: only existing candidate values are replaced.
func prefetchClocks(tx *sql.Tx, user int64, prefix string, clocks map[int64]syncClock) error {
	if len(clocks) == 0 {
		return nil
	}
	const batchSize = 500
	args := make([]any, 1, min(len(clocks), batchSize)+1)
	args[0] = user
	for id := range clocks {
		args = append(args, id)
		if len(args) == batchSize+1 {
			if err := readClockBatch(tx, prefix, args, clocks); err != nil {
				return err
			}
			args = args[:1]
		}
	}
	if len(args) > 1 {
		return readClockBatch(tx, prefix, args, clocks)
	}
	return nil
}

func readFavouriteClockBatch(tx *sql.Tx, args []any, clocks map[favouriteKey]syncClock) error {
	placeholders := strings.TrimSuffix(strings.Repeat("(?,?),", (len(args)-1)/2), ",")
	rows, err := tx.Query(`SELECT manga_id, category_id, created_at, deleted_at FROM sync_favourites WHERE user_id = ? AND (manga_id, category_id) IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key favouriteKey
		var clock syncClock
		if err := rows.Scan(&key.manga, &key.category, &clock.at, &clock.deleted); err != nil {
			return err
		}
		clock.exists = true
		clocks[key] = clock
	}
	return rows.Err()
}

func prefetchFavouriteClocks(tx *sql.Tx, user int64, clocks map[favouriteKey]syncClock) error {
	if len(clocks) == 0 {
		return nil
	}
	// Keep the number of parameters below SQLite's traditional 999 limit.
	const batchSize = 400
	args := make([]any, 1, 2*min(len(clocks), batchSize)+1)
	args[0] = user
	for key := range clocks {
		args = append(args, key.manga, key.category)
		if len(args) == 2*batchSize+1 {
			if err := readFavouriteClockBatch(tx, args, clocks); err != nil {
				return err
			}
			args = args[:1]
		}
	}
	if len(args) > 1 {
		return readFavouriteClockBatch(tx, args, clocks)
	}
	return nil
}

func (a *app) saveHistory(tx *sql.Tx, user int64, input []History) error {
	clocks := make(map[int64]syncClock, len(input))
	for i := range input {
		clocks[input[i].MangaID] = syncClock{}
	}
	query := `INSERT INTO sync_history (user_id, manga_id, created_at, updated_at, chapter_id, page, scroll, percent, chapters, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, manga_id) DO UPDATE SET created_at=excluded.created_at, updated_at=excluded.updated_at, chapter_id=excluded.chapter_id, page=excluded.page, scroll=excluded.scroll, percent=excluded.percent, chapters=excluded.chapters, deleted_at=excluded.deleted_at`
	if a.dialect == "mysql" {
		query = `INSERT INTO sync_history (user_id, manga_id, created_at, updated_at, chapter_id, page, scroll, percent, chapters, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE created_at=VALUES(created_at), updated_at=VALUES(updated_at), chapter_id=VALUES(chapter_id), page=VALUES(page), scroll=VALUES(scroll), percent=VALUES(percent), chapters=VALUES(chapters), deleted_at=VALUES(deleted_at)`
	}
	for i, row := range input {
		if err := a.saveManga(tx, row.Manga); err != nil {
			return err
		}
		// The first clock read originally followed this catalogue write.
		if i == 0 {
			if err := prefetchClocks(tx, user, `SELECT manga_id, updated_at, deleted_at FROM sync_history WHERE user_id = ? AND manga_id IN (`, clocks); err != nil {
				return err
			}
		}
		clock := clocks[row.MangaID]
		if clock.exists {
			if row.UpdatedAt < clock.at {
				continue
			}
			if row.UpdatedAt == clock.at && clock.deleted != 0 && (row.DeletedAt == 0 || row.DeletedAt < clock.deleted) {
				continue
			}
			if clock.deleted != 0 && row.DeletedAt == 0 && row.UpdatedAt <= clock.deleted {
				continue
			}
		}
		if _, err := tx.Exec(query, user, row.MangaID, row.CreatedAt, row.UpdatedAt, row.ChapterID, row.Page, float32(row.Scroll), float32(row.Percent), row.Chapters, row.DeletedAt); err != nil {
			return err
		}
		clocks[row.MangaID] = syncClock{at: row.UpdatedAt, deleted: row.DeletedAt, exists: true}
	}
	return nil
}

// Only a deletion/re-add conflict needs timestamp arbitration; when both copies
// agree on deletion state, normal favourite edits take the submitted values.
func acceptLibraryRow(oldCreated, oldDeleted, newCreated, newDeleted int64) bool {
	if (oldDeleted == 0) == (newDeleted == 0) {
		return true
	}
	oldEvent := oldCreated
	if oldDeleted > oldEvent {
		oldEvent = oldDeleted
	}
	newEvent := newCreated
	if newDeleted > newEvent {
		newEvent = newDeleted
	}
	if oldEvent == newEvent {
		return newDeleted != 0
	}
	return newEvent > oldEvent
}

func (a *app) saveCategory(tx *sql.Tx, user int64, row Category, clocks map[int64]syncClock) error {
	clock := clocks[row.ID]
	if clock.exists && !acceptLibraryRow(clock.at, clock.deleted, row.CreatedAt, row.DeletedAt) {
		return nil
	}
	query := `INSERT INTO sync_categories (user_id, category_id, created_at, sort_key, track, title, sort_order, deleted_at, show_in_lib) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, category_id) DO UPDATE SET created_at=excluded.created_at, sort_key=excluded.sort_key, track=excluded.track, title=excluded.title, sort_order=excluded.sort_order, deleted_at=excluded.deleted_at, show_in_lib=excluded.show_in_lib`
	if a.dialect == "mysql" {
		query = `INSERT INTO sync_categories (user_id, category_id, created_at, sort_key, track, title, sort_order, deleted_at, show_in_lib) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE created_at=VALUES(created_at), sort_key=VALUES(sort_key), track=VALUES(track), title=VALUES(title), sort_order=VALUES(sort_order), deleted_at=VALUES(deleted_at), show_in_lib=VALUES(show_in_lib)`
	}
	if _, err := tx.Exec(query, user, row.ID, row.CreatedAt, row.SortKey, row.Track, truncate(row.Title, 120), row.Order, row.DeletedAt, row.ShowInLib); err != nil {
		return err
	}
	clocks[row.ID] = syncClock{at: row.CreatedAt, deleted: row.DeletedAt, exists: true}
	return nil
}

var errMissingCategory = errors.New("favourite references a missing category")

func (a *app) saveFavourites(tx *sql.Tx, user int64, input []Favourite, categories map[int64]syncClock) error {
	clocks := make(map[favouriteKey]syncClock, len(input))
	for i := range input {
		clocks[favouriteKey{manga: input[i].MangaID, category: input[i].CategoryID}] = syncClock{}
	}
	query := `INSERT INTO sync_favourites (user_id, manga_id, category_id, sort_key, pinned, created_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, manga_id, category_id) DO UPDATE SET sort_key=excluded.sort_key, pinned=excluded.pinned, created_at=excluded.created_at, deleted_at=excluded.deleted_at`
	if a.dialect == "mysql" {
		query = `INSERT INTO sync_favourites (user_id, manga_id, category_id, sort_key, pinned, created_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE sort_key=VALUES(sort_key), pinned=VALUES(pinned), created_at=VALUES(created_at), deleted_at=VALUES(deleted_at)`
	}
	for i, row := range input {
		if !categories[row.CategoryID].exists {
			return errMissingCategory
		}
		if err := a.saveManga(tx, row.Manga); err != nil {
			return err
		}
		// Keep the first clock read after category validation and the manga write.
		if i == 0 {
			if err := prefetchFavouriteClocks(tx, user, clocks); err != nil {
				return err
			}
		}
		key := favouriteKey{manga: row.MangaID, category: row.CategoryID}
		clock := clocks[key]
		if clock.exists && !acceptLibraryRow(clock.at, clock.deleted, row.CreatedAt, row.DeletedAt) {
			continue
		}
		if _, err := tx.Exec(query, user, row.MangaID, row.CategoryID, row.SortKey, row.Pinned, row.CreatedAt, row.DeletedAt); err != nil {
			return err
		}
		clocks[key] = syncClock{at: row.CreatedAt, deleted: row.DeletedAt, exists: true}
	}
	return nil
}

func equalManga(a, b Manga) bool {
	if len(a.Tags) != len(b.Tags) {
		return false
	}
	atags := append([]MangaTag(nil), a.Tags...)
	btags := append([]MangaTag(nil), b.Tags...)
	sort.Slice(atags, func(i, j int) bool { return atags[i].ID < atags[j].ID })
	sort.Slice(btags, func(i, j int) bool { return btags[i].ID < btags[j].ID })
	a.Tags, b.Tags = nil, nil
	return reflect.DeepEqual(a, b) && reflect.DeepEqual(atags, btags)
}

func equalHistory(a, b []History) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if !equalManga(x.Manga, y.Manga) {
			return false
		}
		x.Manga, y.Manga = Manga{}, Manga{}
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

func equalFavourites(a, b []Favourite) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if !equalManga(x.Manga, y.Manga) {
			return false
		}
		x.Manga, y.Manga = Manga{}, Manga{}
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

func syncFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, errMissingCategory) {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("sync: %v", err)
	respondError(w, http.StatusInternalServerError, "Internal server error")
}

func (a *app) registerSync(mux *http.ServeMux) {
	mux.HandleFunc("GET /resource/history", a.historyHandler)
	mux.HandleFunc("POST /resource/history", a.historyHandler)
	mux.HandleFunc("GET /resource/favourites", a.favouritesHandler)
	mux.HandleFunc("POST /resource/favourites", a.favouritesHandler)
}

func (a *app) historyHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := a.userID(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var input []History
	var err error
	if r.Method == http.MethodPost {
		input, err = decodeHistory(r)
		if err != nil {
			respondError(w, http.StatusBadRequest, "Invalid sync package")
			return
		}
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		syncFailure(w, err)
		return
	}
	defer tx.Rollback()
	if r.Method == http.MethodPost {
		if err = a.lockUser(tx, user); err != nil {
			syncFailure(w, err)
			return
		}
		if err = a.saveHistory(tx, user, input); err != nil {
			syncFailure(w, err)
			return
		}
	}
	result, err := readHistory(tx, user)
	if err != nil {
		syncFailure(w, err)
		return
	}
	if r.Method == http.MethodPost {
		if err = a.setTimestamp(tx, user, true); err != nil {
			syncFailure(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		syncFailure(w, err)
		return
	}
	if r.Method == http.MethodPost && equalHistory(result.History, input) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

func (a *app) favouritesHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := a.userID(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var categories []Category
	var favourites []Favourite
	var err error
	if r.Method == http.MethodPost {
		categories, favourites, err = decodeFavourites(r)
		if err != nil {
			respondError(w, http.StatusBadRequest, "Invalid sync package")
			return
		}
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		syncFailure(w, err)
		return
	}
	defer tx.Rollback()
	if r.Method == http.MethodPost {
		if err = a.lockUser(tx, user); err != nil {
			syncFailure(w, err)
			return
		}
		// Category rows are never physically removed. Updating this cache after
		// each accepted upsert also makes it the post-category existence set.
		categoryClocks := make(map[int64]syncClock, len(categories))
		for i := range categories {
			categoryClocks[categories[i].ID] = syncClock{}
		}
		for i := range favourites {
			categoryClocks[favourites[i].CategoryID] = syncClock{}
		}
		if err = prefetchClocks(tx, user, `SELECT category_id, created_at, deleted_at FROM sync_categories WHERE user_id = ? AND category_id IN (`, categoryClocks); err != nil {
			syncFailure(w, err)
			return
		}
		for _, row := range categories {
			if err = a.saveCategory(tx, user, row, categoryClocks); err != nil {
				syncFailure(w, err)
				return
			}
		}
		if err = a.saveFavourites(tx, user, favourites, categoryClocks); err != nil {
			syncFailure(w, err)
			return
		}
	}
	result, err := readFavourites(tx, user)
	if err != nil {
		syncFailure(w, err)
		return
	}
	if r.Method == http.MethodPost {
		if err = a.setTimestamp(tx, user, false); err != nil {
			syncFailure(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		syncFailure(w, err)
		return
	}
	if r.Method == http.MethodPost && reflect.DeepEqual(result.Categories, categories) && equalFavourites(result.Favourites, favourites) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	respondJSON(w, http.StatusOK, result)
}
