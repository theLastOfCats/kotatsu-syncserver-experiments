package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

type mangaTag struct {
	ID     int64  `json:"tag_id"`
	Title  string `json:"title"`
	Key    string `json:"key"`
	Source string `json:"source"`
	Pinned bool   `json:"pinned"`
}

type manga struct {
	ID            int64      `json:"manga_id"`
	Title         string     `json:"title"`
	AltTitle      *string    `json:"alt_title"`
	URL           string     `json:"url"`
	PublicURL     string     `json:"public_url"`
	Rating        float64    `json:"rating"`
	ContentRating *string    `json:"content_rating"`
	CoverURL      string     `json:"cover_url"`
	LargeCoverURL *string    `json:"large_cover_url"`
	Tags          []mangaTag `json:"tags"`
	State         *string    `json:"state"`
	Author        *string    `json:"author"`
	Source        string     `json:"source"`
	NSFW          bool       `json:"nsfw"`
}

type historyRow struct {
	MangaID   int64   `json:"manga_id"`
	Manga     manga   `json:"manga"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
	ChapterID int64   `json:"chapter_id"`
	Page      int     `json:"page"`
	Scroll    float64 `json:"scroll"`
	Percent   float64 `json:"percent"`
	Chapters  int     `json:"chapters"`
	DeletedAt int64   `json:"deleted_at"`
}

type category struct {
	ID        int    `json:"category_id"`
	CreatedAt int64  `json:"created_at"`
	SortKey   int    `json:"sort_key"`
	Track     bool   `json:"track"`
	Title     string `json:"title"`
	Order     string `json:"order"`
	DeletedAt int64  `json:"deleted_at"`
	ShowInLib bool   `json:"show_in_lib"`
}

type favourite struct {
	MangaID    int64 `json:"manga_id"`
	Manga      manga `json:"manga"`
	CategoryID int   `json:"category_id"`
	SortKey    int   `json:"sort_key"`
	Pinned     bool  `json:"pinned"`
	CreatedAt  int64 `json:"created_at"`
	DeletedAt  int64 `json:"deleted_at"`
}

type syncPacket struct {
	History    []historyRow `json:"history"`
	Categories []category   `json:"categories"`
	Favourites []favourite  `json:"favourites"`
	Timestamp  int64        `json:"timestamp"`
}

type worker struct {
	client          *apiClient
	token           string
	history         []historyRow
	categories      []category
	favourites      []favourite
	clock           int64
	historyTurn     int
	favouriteTurn   int
	lastHistoryBody []byte
}

func newWorker(client *apiClient, token string, userID int64, library int) *worker {
	w := newHistoryWorker(client, token, userID, library)
	categoryCount := min(library, 5)
	w.categories = make([]category, categoryCount)
	for i := range w.categories {
		w.categories[i] = category{
			ID: i + 1, CreatedAt: w.clock, SortKey: i, Track: true,
			Title: fmt.Sprintf("Benchmark shelf %d", i+1), Order: "MANUAL", ShowInLib: true,
		}
	}
	w.favourites = make([]favourite, 0, library)
	for i := range w.history {
		row := &w.history[i]
		w.favourites = append(w.favourites, favourite{
			MangaID: row.MangaID, Manga: row.Manga,
			CategoryID: int(int64(i)*int64(categoryCount)/int64(library)) + 1,
			SortKey:    i * 2, Pinned: i%2 == 0, CreatedAt: w.clock,
		})
	}
	return w
}

func newHistoryWorker(client *apiClient, token string, userID int64, library int) *worker {
	created := time.Now().UnixMilli() - 1000
	w := &worker{
		client:  client,
		token:   token,
		history: make([]historyRow, 0, library),
		clock:   created,
	}
	for i := range library {
		// Account IDs are server-assigned and never reused within a database.
		// Reserve the low 32 bits for the row number, so different accounts and
		// successive benchmark invocations cannot publish the same manga IDs.
		id := (userID << 32) | int64(i+1)
		emptyCover := ""
		card := manga{
			ID: id, Title: fmt.Sprintf("Benchmark manga %d", i+1),
			URL:       fmt.Sprintf("/benchmark/%d", id),
			PublicURL: fmt.Sprintf("https://example.invalid/benchmark/%d", id),
			Rating:    0.5, CoverURL: "https://example.invalid/cover.jpg",
			LargeCoverURL: &emptyCover, Tags: []mangaTag{}, Source: "EXAMPLE",
		}
		w.history = append(w.history, historyRow{
			MangaID: id, Manga: card, CreatedAt: created, UpdatedAt: created,
			ChapterID: int64(i + 1), Page: 1, Scroll: 0.5, Percent: 0.25, Chapters: 12,
		})
	}
	return w
}

func (w *worker) historyPacket() syncPacket {
	return syncPacket{History: w.history, Timestamp: w.clock}
}

func (w *worker) favouritesPacket() syncPacket {
	return syncPacket{Categories: w.categories, Favourites: w.favourites, Timestamp: w.clock}
}

func (w *worker) checkHistory(body []byte) error {
	var got syncPacket
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("decode history response: %w", err)
	}
	if got.History == nil || len(got.History) != len(w.history) {
		return fmt.Errorf("history row count: got %d, want %d", len(got.History), len(w.history))
	}
	rows := make(map[int64]historyRow, len(got.History))
	for _, row := range got.History {
		if _, exists := rows[row.MangaID]; exists {
			return fmt.Errorf("duplicate history manga_id %d", row.MangaID)
		}
		rows[row.MangaID] = row
	}
	for _, want := range w.history {
		got, exists := rows[want.MangaID]
		if !exists || !reflect.DeepEqual(got, want) {
			return fmt.Errorf("history manga_id %d is missing or differs (page %d, updated_at %d)", want.MangaID, want.Page, want.UpdatedAt)
		}
	}
	return nil
}

func (w *worker) checkFavourites(body []byte) error {
	var got syncPacket
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("decode favourites response: %w", err)
	}
	if got.Categories == nil || len(got.Categories) != len(w.categories) {
		return fmt.Errorf("category count: got %d, want %d", len(got.Categories), len(w.categories))
	}
	if got.Favourites == nil || len(got.Favourites) != len(w.favourites) {
		return fmt.Errorf("favourite count: got %d, want %d", len(got.Favourites), len(w.favourites))
	}
	categories := make(map[int]category, len(got.Categories))
	for _, row := range got.Categories {
		if _, exists := categories[row.ID]; exists {
			return fmt.Errorf("duplicate category_id %d", row.ID)
		}
		categories[row.ID] = row
	}
	for _, want := range w.categories {
		got, exists := categories[want.ID]
		if !exists || got != want {
			return fmt.Errorf("category_id %d is missing or differs", want.ID)
		}
	}
	favourites := make(map[int64]favourite, len(got.Favourites))
	for _, row := range got.Favourites {
		if _, exists := favourites[row.MangaID]; exists {
			return fmt.Errorf("duplicate favourite manga_id %d", row.MangaID)
		}
		favourites[row.MangaID] = row
	}
	for _, want := range w.favourites {
		got, exists := favourites[want.MangaID]
		if !exists || !reflect.DeepEqual(got, want) {
			return fmt.Errorf("favourite manga_id %d is missing or differs (category_id %d, sort_key %d)", want.MangaID, want.CategoryID, want.SortKey)
		}
	}
	return nil
}
