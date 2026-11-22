package main

import "net/http"

// These stories record reference-server defects, not requirements for a replacement.
func syncObservations(r *run) {
	const created int64 = 1710000000000
	const updated int64 = 1710000001000
	const deleted int64 = 1710000002000
	const categoryID = 91001
	const missingCategoryID = 91002
	const favouriteMangaID = 910001
	const invalidMangaID = 910002
	const historyMangaID = 910003

	manga := func(id int, title string) map[string]any {
		return map[string]any{
			"manga_id": id, "title": title, "alt_title": nil,
			"url": "/manga/observation", "public_url": "https://example.invalid/manga/observation",
			"rating": 0.5, "content_rating": nil, "cover_url": "https://example.invalid/cover.jpg",
			"large_cover_url": "", "tags": []any{}, "state": nil, "author": nil,
			"source": "EXAMPLE", "nsfw": false,
		}
	}
	category := map[string]any{
		"category_id": categoryID, "created_at": created, "sort_key": 1,
		"track": true, "title": "Observation category", "order": "MANUAL", "deleted_at": int64(0), "show_in_lib": true,
	}
	favourite := func(id, category int) map[string]any {
		return map[string]any{
			"manga_id": id, "manga": manga(id, "Observation favourite"), "category_id": category,
			"sort_key": 1, "pinned": true, "created_at": created, "deleted_at": int64(0),
		}
	}

	r.story("observation_favourites_partial_write")
	auth := storyObject(r, r.step("Given an observation reader signs in", http.MethodPost, "/auth", "", map[string]any{
		"email": "contract-observation-sync@example.invalid", "password": "contract-password",
	}, http.StatusOK))
	token, ok := auth["token"].(string)
	r.require(ok && token != "", "authentication must return a nonempty token")
	checkEmpty(r, r.step("And the reader starts with an empty library", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK), "categories", "favourites")
	r.step("When a later favourite references a missing category", http.MethodPost, "/resource/favourites", token, map[string]any{
		"history": nil, "categories": []any{category},
		"favourites": []any{favourite(favouriteMangaID, categoryID), favourite(invalidMangaID, missingCategoryID)},
		"timestamp":  deleted,
	}, http.StatusInternalServerError)
	persisted := r.step("Then the earlier category and favourite remain persisted", http.MethodGet, "/resource/favourites", token, nil, http.StatusOK)
	storedCategory := storyRow(r, persisted, "categories")
	storyNumber(r, storedCategory, "category_id", categoryID)
	storyNumber(r, storedCategory, "created_at", created)
	r.require(storedCategory["title"] == "Observation category", "persisted category title differs")
	storedFavourite := storyRow(r, persisted, "favourites")
	storyNumber(r, storedFavourite, "manga_id", favouriteMangaID)
	storyNumber(r, storedFavourite, "category_id", categoryID)
	storyNumber(r, storedFavourite, "created_at", created)
	storyNumber(r, storedFavourite, "deleted_at", 0)
	rejectedCard := storyObject(r, r.step("And the rejected favourite's manga was already published", http.MethodGet, "/manga/910002", "", nil, http.StatusOK))
	storyNumber(r, rejectedCard, "manga_id", invalidMangaID)

	r.story("observation_history_equal_timestamp_revives")
	historyRow := func(removed int64) map[string]any {
		return map[string]any{
			"manga_id": historyMangaID, "manga": manga(historyMangaID, "Observation history"),
			"created_at": created, "updated_at": updated, "chapter_id": 91,
			"page": 4, "scroll": 0.5, "percent": 0.25, "chapters": 10, "deleted_at": removed,
		}
	}
	historyPackage := func(removed int64) map[string]any {
		return map[string]any{"history": []any{historyRow(removed)}, "categories": nil, "favourites": nil, "timestamp": deleted}
	}
	checkHistory := func(body any, removed int64) {
		row := storyRow(r, body, "history")
		storyNumber(r, row, "manga_id", historyMangaID)
		storyNumber(r, row, "updated_at", updated)
		storyNumber(r, row, "deleted_at", removed)
		storyNumber(r, row, "page", 4)
	}
	checkEmpty(r, r.step("Given the reader starts without history", http.MethodGet, "/resource/history", token, nil, http.StatusOK), "history")
	body := r.step("When a phone uploads its active history", http.MethodPost, "/resource/history", token, historyPackage(0), http.StatusNoContent)
	r.require(body == nil, "204 history upload must have no body")
	body = r.step("And another phone deletes it without advancing updated_at", http.MethodPost, "/resource/history", token, historyPackage(deleted), http.StatusNoContent)
	r.require(body == nil, "204 history deletion must have no body")
	checkHistory(r.step("Then the server stores the tombstone", http.MethodGet, "/resource/history", token, nil, http.StatusOK), deleted)
	body = r.step("When the offline phone uploads its equally updated active row", http.MethodPost, "/resource/history", token, historyPackage(0), http.StatusNoContent)
	r.require(body == nil, "204 stale history upload must have no body")
	checkHistory(r.step("Then the legacy server has revived the active row", http.MethodGet, "/resource/history", token, nil, http.StatusOK), 0)

	r.story("observation_invalid_sync_json")
	r.step("When the phone sends a truncated JSON object", http.MethodPost, "/resource/history", token,
		rawJSON(`{"history":[`), http.StatusInternalServerError)
	r.step("And when the history field has the wrong JSON type", http.MethodPost, "/resource/history", token,
		map[string]any{"history": "not an array", "timestamp": deleted}, http.StatusInternalServerError)
	checkHistory(r.step("Then neither parse failure changes the stored history", http.MethodGet, "/resource/history", token, nil, http.StatusOK), 0)
}
