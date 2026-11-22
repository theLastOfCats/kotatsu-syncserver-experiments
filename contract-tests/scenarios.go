package main

import (
	"encoding/json"
	"net/http"
)

// The fixed clocks represent ordering, not wall-clock time. Each run needs a
// freshly initialized reference database so these accounts start empty.
func scenarios(r *run) {
	const added int64 = 1700000000000
	const progressed int64 = 1700000001000
	const deleted int64 = 1700000002000
	const readded int64 = 1700000003000
	const mangaID = 900001
	const categoryID = 9001

	manga := map[string]any{
		"manga_id": mangaID, "title": "A synthetic manga", "alt_title": nil,
		"url": "/manga/synthetic", "public_url": "https://example.invalid/manga/synthetic",
		"rating": 0.5, "content_rating": nil, "cover_url": "https://example.invalid/cover.jpg",
		"large_cover_url": "", "tags": []any{map[string]any{
			"tag_id": 90001, "title": "Adventure", "key": "adventure", "source": "EXAMPLE", "pinned": false,
		}},
		"state": nil, "author": nil, "source": "EXAMPLE", "nsfw": false,
	}
	historyRow := func(updated, removed int64, page int) map[string]any {
		return map[string]any{
			"manga_id": mangaID, "manga": manga, "created_at": added, "updated_at": updated,
			"chapter_id": 77, "page": page, "scroll": 0.5, "percent": 0.25, "chapters": 12,
			"deleted_at": removed,
		}
	}
	historyPackage := func(rows ...any) map[string]any {
		return map[string]any{"history": rows, "categories": nil, "favourites": nil, "timestamp": readded}
	}
	category := func(created, removed int64) map[string]any {
		return map[string]any{
			"category_id": categoryID, "created_at": created, "sort_key": 1,
			"track": true, "title": "Reading", "order": "MANUAL", "deleted_at": removed, "show_in_lib": true,
		}
	}
	favourite := func(created, removed int64) map[string]any {
		return map[string]any{
			"manga_id": mangaID, "manga": manga, "category_id": categoryID, "sort_key": 1,
			"pinned": true, "created_at": created, "deleted_at": removed,
		}
	}
	favouritesPackage := func(categories, favourites []any) map[string]any {
		return map[string]any{"history": nil, "categories": categories, "favourites": favourites, "timestamp": readded}
	}
	emptyFavourites := func() map[string]any { return favouritesPackage([]any{}, []any{}) }
	singleFavourites := func(created, removed int64) map[string]any {
		return favouritesPackage([]any{category(created, removed)}, []any{favourite(created, removed)})
	}
	checkHistory := func(body any, updated, removed int64, page int) {
		row := storyRow(r, body, "history")
		storyNumber(r, row, "manga_id", mangaID)
		storyNumber(r, row, "updated_at", updated)
		storyNumber(r, row, "deleted_at", removed)
		storyNumber(r, row, "page", int64(page))
		card := storyObject(r, row["manga"])
		storyNumber(r, card, "manga_id", mangaID)
		r.require(card["title"] == "A synthetic manga", "history manga title differs")
		tags, ok := card["tags"].([]any)
		r.require(ok && len(tags) == 1, "history manga should have one tag")
		tag := storyObject(r, tags[0])
		r.require(tag["key"] == "adventure", "history manga tag differs")
		cover, exists := card["large_cover_url"]
		r.require(exists && cover == "", "history manga large cover should be empty")
	}
	checkFavourites := func(body any, created, removed int64) {
		cat := storyRow(r, body, "categories")
		storyNumber(r, cat, "category_id", categoryID)
		storyNumber(r, cat, "created_at", created)
		storyNumber(r, cat, "deleted_at", removed)
		r.require(cat["title"] == "Reading", "category title differs")
		fav := storyRow(r, body, "favourites")
		storyNumber(r, fav, "manga_id", mangaID)
		storyNumber(r, fav, "category_id", categoryID)
		storyNumber(r, fav, "created_at", created)
		storyNumber(r, fav, "deleted_at", removed)
		r.require(fav["pinned"] == true, "favourite should be pinned")
	}

	r.story("history_two_phones")
	auth := storyObject(r, r.step("Given the reader signs in", http.MethodPost, "/auth", "", map[string]any{
		"email": "contract-reader@example.invalid", "password": "contract-password",
	}, http.StatusOK))
	token, ok := auth["token"].(string)
	r.require(ok && token != "", "authentication must return a nonempty token")

	body := r.step("When the first phone uploads reading progress", http.MethodPost, "/resource/history", token,
		historyPackage(historyRow(added, 0, 3)), http.StatusNoContent)
	r.require(body == nil, "204 history upload must have no body")
	checkHistory(r.step("Then the stored history contains that progress", http.MethodGet, "/resource/history", token, nil, http.StatusOK), added, 0, 3)
	checkHistory(r.step("When an empty second phone syncs it learns the first phone's history", http.MethodPost, "/resource/history", token,
		historyPackage([]any{}...), http.StatusOK), added, 0, 3)
	body = r.step("When the first phone advances", http.MethodPost, "/resource/history", token,
		historyPackage(historyRow(progressed, 0, 8)), http.StatusNoContent)
	r.require(body == nil, "204 history advance must have no body")
	checkHistory(r.step("Then the stale second phone receives newer progress", http.MethodPost, "/resource/history", token,
		historyPackage(historyRow(added, 0, 3)), http.StatusOK), progressed, 0, 8)
	body = r.step("When the first phone deletes history without advancing updated_at", http.MethodPost, "/resource/history", token,
		historyPackage(historyRow(progressed, deleted, 8)), http.StatusNoContent)
	r.require(body == nil, "204 history deletion must have no body")
	checkHistory(r.step("Then another phone receives the reading-history deletion", http.MethodPost, "/resource/history", token,
		historyPackage([]any{}...), http.StatusOK), progressed, deleted, 8)
	card := storyObject(r, r.step("And the catalog exposes the manga card", http.MethodGet, "/manga/900001", "", nil, http.StatusOK))
	storyNumber(r, card, "manga_id", mangaID)
	r.require(card["title"] == "A synthetic manga", "catalog manga title differs")

	r.story("favourites_two_phones")
	checkEmpty(r, r.step("Given the first phone reads its library before uploading", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK), "categories", "favourites")
	body = r.step("When the first phone adds a category and favourite", http.MethodPost, "/resource/favourites", token,
		singleFavourites(added, 0), http.StatusNoContent)
	r.require(body == nil, "204 favourites upload must have no body")
	checkFavourites(r.step("Given the second phone reads the library before its first upload", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK), added, 0)
	checkFavourites(r.step("When the empty second phone posts its library it learns both rows", http.MethodPost, "/resource/favourites", token,
		emptyFavourites(), http.StatusOK), added, 0)
	checkFavourites(r.step("Given the first phone reads before deleting", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK), added, 0)
	body = r.step("When the first phone deletes its category and favourite", http.MethodPost, "/resource/favourites", token,
		singleFavourites(added, deleted), http.StatusNoContent)
	r.require(body == nil, "204 favourites deletion must have no body")
	remote := r.step("Then the stale second phone reads the tombstones before posting", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK)
	checkFavourites(remote, added, deleted)
	// Android applies newer remote deletions to locally known keys before POST.
	remotePackage := storyObject(r, remote)
	body = r.step("When the second phone posts its pre-applied tombstones", http.MethodPost, "/resource/favourites", token,
		favouritesPackage(remotePackage["categories"].([]any), remotePackage["favourites"].([]any)), http.StatusNoContent)
	r.require(body == nil, "204 reconciled favourites upload must have no body")
	checkFavourites(r.step("Given the first phone reads the deleted library", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK), added, deleted)
	body = r.step("When the first phone adds the category and favourite again later", http.MethodPost, "/resource/favourites", token,
		singleFavourites(readded, 0), http.StatusNoContent)
	r.require(body == nil, "204 favourites re-add must have no body")
	checkFavourites(r.step("Then the second phone observes the newer re-add before posting", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK), readded, 0)

	r.story("account_isolation")
	r.step("Given an anonymous visitor requests history", http.MethodGet, "/resource/history", "", nil, http.StatusUnauthorized)
	r.step("And an anonymous visitor requests favourites", http.MethodGet, "/resource/favourites", "", nil, http.StatusUnauthorized)
	otherAuth := storyObject(r, r.step("When a separate reader signs in", http.MethodPost, "/auth", "", map[string]any{
		"email": "contract-other@example.invalid", "password": "contract-password",
	}, http.StatusOK))
	otherToken, ok := otherAuth["token"].(string)
	r.require(ok && otherToken != "", "authentication must return a nonempty token")
	checkEmpty(r, r.step("Then the separate reader cannot see the first reader's history", http.MethodGet, "/resource/history", otherToken, nil, http.StatusOK), "history")
	checkEmpty(r, r.step("And the separate reader cannot see the first reader's categories or favourites", http.MethodGet, "/resource/favourites", otherToken, nil, http.StatusOK), "categories", "favourites")
}

func storyObject(r *run, value any) map[string]any {
	object, ok := value.(map[string]any)
	r.require(ok, "expected JSON object")
	return object
}

func storyRow(r *run, body any, key string) map[string]any {
	object := storyObject(r, body)
	rows, ok := object[key].([]any)
	r.require(ok && len(rows) == 1, "expected exactly one %s row", key)
	return storyObject(r, rows[0])
}

func storyNumber(r *run, object map[string]any, key string, expected int64) {
	number, ok := object[key].(json.Number)
	r.require(ok, "expected numeric %s", key)
	actual, err := number.Int64()
	r.require(err == nil && actual == expected, "unexpected %s: got %v, want %d", key, number, expected)
}

func checkEmpty(r *run, body any, keys ...string) {
	object := storyObject(r, body)
	for _, key := range keys {
		rows, ok := object[key].([]any)
		r.require(ok && len(rows) == 0, "expected empty %s", key)
	}
}
