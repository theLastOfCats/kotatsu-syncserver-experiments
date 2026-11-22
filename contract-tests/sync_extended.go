package main

import (
	"encoding/json"
	"net/http"
)

// These fixed clocks model phones coming back after Android's four-day local
// tombstone GC period; they do not depend on the server's wall clock.
func additionalSync(r *run) {
	const added int64 = 1600000000000
	const progressed int64 = added + 1000
	const deleted int64 = added + 2000
	const afterOffline int64 = deleted + 5*24*60*60*1000
	const readded int64 = afterOffline + 1000

	type libraryEntry struct {
		mangaID    int64
		categoryID int64
		label      string
		created    int64
		deleted    int64
	}

	manga := func(id int64, title string, largeCover any) map[string]any {
		return map[string]any{
			"manga_id": id, "title": title, "alt_title": nil,
			"url": "/manga/sync-contract", "public_url": "https://example.invalid/manga/sync-contract",
			"rating": 0.5, "content_rating": nil, "cover_url": "https://example.invalid/cover.jpg",
			"large_cover_url": largeCover, "tags": []any{}, "state": nil, "author": nil,
			"source": "EXAMPLE", "nsfw": false,
		}
	}
	historyRow := func(id int64, title string, created, updated, removed int64, page int, largeCover any) map[string]any {
		return map[string]any{
			"manga_id": id, "manga": manga(id, title, largeCover), "created_at": created,
			"updated_at": updated, "chapter_id": int64(77), "page": page, "scroll": 0.5,
			"percent": 0.25, "chapters": 12, "deleted_at": removed,
		}
	}
	historyPackage := func(timestamp int64, rows ...any) map[string]any {
		return map[string]any{"history": rows, "categories": nil, "favourites": nil, "timestamp": timestamp}
	}
	category := func(entry libraryEntry) map[string]any {
		return map[string]any{
			"category_id": entry.categoryID, "created_at": entry.created, "sort_key": 1,
			"track": true, "title": entry.label + " shelf", "order": "MANUAL",
			"deleted_at": entry.deleted, "show_in_lib": true,
		}
	}
	favourite := func(entry libraryEntry) map[string]any {
		return map[string]any{
			"manga_id": entry.mangaID, "manga": manga(entry.mangaID, entry.label+" manga", ""),
			"category_id": entry.categoryID, "sort_key": 1, "pinned": true,
			"created_at": entry.created, "deleted_at": entry.deleted,
		}
	}
	libraryPackage := func(timestamp int64, categories, favourites []any) map[string]any {
		return map[string]any{"history": nil, "categories": categories, "favourites": favourites, "timestamp": timestamp}
	}
	libraryOf := func(timestamp int64, entries ...libraryEntry) map[string]any {
		categories := make([]any, 0, len(entries))
		favourites := make([]any, 0, len(entries))
		for _, entry := range entries {
			categories = append(categories, category(entry))
			favourites = append(favourites, favourite(entry))
		}
		return libraryPackage(timestamp, categories, favourites)
	}
	authenticate := func(email string) string {
		body := storyObject(r, r.step("Given the reader signs in", http.MethodPost, "/auth", "", map[string]any{
			"email": email, "password": "contract-password",
		}, http.StatusOK))
		token, ok := body["token"].(string)
		r.require(ok && token != "", "authentication must return a nonempty token")
		return token
	}
	noContent := func(body any) {
		r.require(body == nil, "204 sync response must have no body")
	}
	checkHistory := func(body any, id int64, title string, created, updated, removed int64, page int) {
		row := storyRow(r, body, "history")
		storyNumber(r, row, "manga_id", id)
		storyNumber(r, row, "created_at", created)
		storyNumber(r, row, "updated_at", updated)
		storyNumber(r, row, "deleted_at", removed)
		storyNumber(r, row, "page", int64(page))
		card := storyObject(r, row["manga"])
		storyNumber(r, card, "manga_id", id)
		r.require(card["title"] == title, "history manga title differs")
		cover, exists := card["large_cover_url"]
		r.require(exists && cover == "", "history manga large cover should be an empty string")
	}
	indexRows := func(body any, key, idKey string, count int) map[int64]map[string]any {
		object := storyObject(r, body)
		rows, ok := object[key].([]any)
		r.require(ok && len(rows) == count, "expected exactly %d %s rows", count, key)
		indexed := make(map[int64]map[string]any, len(rows))
		for _, value := range rows {
			row := storyObject(r, value)
			number, ok := row[idKey].(json.Number)
			r.require(ok, "expected numeric %s in %s", idKey, key)
			id, err := number.Int64()
			r.require(err == nil, "expected integer %s in %s", idKey, key)
			_, duplicate := indexed[id]
			r.require(!duplicate, "duplicate %s %d in %s", idKey, id, key)
			indexed[id] = row
		}
		return indexed
	}
	checkLibrary := func(body any, expected ...libraryEntry) {
		categories := indexRows(body, "categories", "category_id", len(expected))
		favourites := indexRows(body, "favourites", "manga_id", len(expected))
		for _, entry := range expected {
			cat, ok := categories[entry.categoryID]
			r.require(ok, "missing category %d", entry.categoryID)
			storyNumber(r, cat, "created_at", entry.created)
			storyNumber(r, cat, "deleted_at", entry.deleted)
			r.require(cat["title"] == entry.label+" shelf", "category title differs")
			fav, ok := favourites[entry.mangaID]
			r.require(ok, "missing favourite %d", entry.mangaID)
			storyNumber(r, fav, "category_id", entry.categoryID)
			storyNumber(r, fav, "created_at", entry.created)
			storyNumber(r, fav, "deleted_at", entry.deleted)
			r.require(fav["pinned"] == true, "favourite should remain pinned")
			card := storyObject(r, fav["manga"])
			storyNumber(r, card, "manga_id", entry.mangaID)
			r.require(card["title"] == entry.label+" manga", "favourite manga title differs")
		}
	}

	const firstManga int64 = 920001
	const secondManga int64 = 920002
	first := libraryEntry{firstManga, 9201, "First", added, 0}
	second := libraryEntry{secondManga, 9202, "Second", added, 0}

	r.story("library_disjoint_snapshots")
	mergeToken := authenticate("contract-merge@example.invalid")
	checkEmpty(r, r.step("Given the first phone preflights its empty library", http.MethodGet, "/resource/favourites", mergeToken, nil, http.StatusOK), "categories", "favourites")
	noContent(r.step("When the first phone uploads its own manga and category", http.MethodPost, "/resource/favourites", mergeToken,
		libraryOf(added, first), http.StatusNoContent))
	// Android only pre-applies remote changes for keys already on the phone.
	checkLibrary(r.step("And the second phone preflights before sending its different local manga", http.MethodGet, "/resource/favourites", mergeToken, nil, http.StatusOK), first)
	checkLibrary(r.step("Then its disjoint upload returns both phones' rows", http.MethodPost, "/resource/favourites", mergeToken,
		libraryOf(progressed, second), http.StatusOK), first, second)
	merged := r.step("And the first phone pulls the merged library", http.MethodGet, "/resource/favourites", mergeToken, nil, http.StatusOK)
	checkLibrary(merged, first, second)
	mergedObject := storyObject(r, merged)
	noContent(r.step("When the first phone uploads the merged library", http.MethodPost, "/resource/favourites", mergeToken,
		libraryPackage(afterOffline, mergedObject["categories"].([]any), mergedObject["favourites"].([]any)), http.StatusNoContent))
	checkLibrary(r.step("Then both sets of rows remain persisted", http.MethodGet, "/resource/favourites", mergeToken, nil, http.StatusOK), first, second)

	r.story("history_disjoint_snapshots")
	firstHistory := historyRow(first.mangaID, first.label+" manga", added, added, 0, 3, "")
	secondHistory := historyRow(second.mangaID, second.label+" manga", added, progressed, 0, 7, "")
	checkMergedHistory := func(body any) {
		rows := indexRows(body, "history", "manga_id", 2)
		storyNumber(r, rows[first.mangaID], "page", 3)
		storyNumber(r, rows[second.mangaID], "page", 7)
	}
	noContent(r.step("Given the first phone uploads its only reading entry", http.MethodPost, "/resource/history", mergeToken,
		historyPackage(added, firstHistory), http.StatusNoContent))
	checkMergedHistory(r.step("When the second phone uploads a different title it receives both entries", http.MethodPost, "/resource/history", mergeToken,
		historyPackage(progressed, secondHistory), http.StatusOK))
	checkMergedHistory(r.step("Then the first phone also receives both entries on its next sync", http.MethodPost, "/resource/history", mergeToken,
		historyPackage(afterOffline, firstHistory), http.StatusOK))

	const replayManga int64 = 920003
	const replayTitle = "Replay manga"
	replay := libraryEntry{replayManga, 9203, "Replay", added, 0}
	r.story("history_upload_replay")
	replayToken := authenticate("contract-replay@example.invalid")
	replayHistory := historyPackage(added, historyRow(replayManga, replayTitle, added, added, 0, 3, ""))
	noContent(r.step("When a phone first uploads its history", http.MethodPost, "/resource/history", replayToken, replayHistory, http.StatusNoContent))
	checkHistory(r.step("Then the initial 204 still persisted the row", http.MethodGet, "/resource/history", replayToken, nil, http.StatusOK), replayManga, replayTitle, added, added, 0, 3)
	noContent(r.step("When the phone replays the identical history", http.MethodPost, "/resource/history", replayToken, replayHistory, http.StatusNoContent))
	checkHistory(r.step("Then the repeated 204 leaves exactly one persisted row", http.MethodGet, "/resource/history", replayToken, nil, http.StatusOK), replayManga, replayTitle, added, added, 0, 3)

	r.story("favourites_upload_replay")
	checkEmpty(r, r.step("Given the phone preflights its empty library", http.MethodGet, "/resource/favourites", replayToken, nil, http.StatusOK), "categories", "favourites")
	replayLibrary := libraryOf(added, replay)
	noContent(r.step("When the phone first uploads a category and favourite", http.MethodPost, "/resource/favourites", replayToken, replayLibrary, http.StatusNoContent))
	checkLibrary(r.step("Then the initial 204 still persisted both rows", http.MethodGet, "/resource/favourites", replayToken, nil, http.StatusOK), replay)
	noContent(r.step("When the phone replays the identical library", http.MethodPost, "/resource/favourites", replayToken, replayLibrary, http.StatusNoContent))
	checkLibrary(r.step("Then the repeated 204 leaves exactly one of each row", http.MethodGet, "/resource/favourites", replayToken, nil, http.StatusOK), replay)

	r.story("wire_double_slash_and_large_cover")
	checkHistory(r.step("Given a double-slash history pull", http.MethodGet, "//resource/history", replayToken, nil, http.StatusOK), replayManga, replayTitle, added, added, 0, 3)
	// The DTO accepts null and SQL permits it, but the reference reads the
	// nullable column through a non-null Kotlin entity property. Its wire value
	// is therefore empty: null differs from that result and returns 200, while
	// an empty-string upload is content-equal and returns 204.
	checkHistory(r.step("When the phone uploads a null large cover through the raw double-slash path", http.MethodPost, "//resource/history", replayToken,
		historyPackage(progressed, historyRow(replayManga, replayTitle, added, added, 0, 3, nil)), http.StatusOK), replayManga, replayTitle, added, added, 0, 3)
	checkHistory(r.step("Then a normal pull exposes the normalized empty large cover", http.MethodGet, "/resource/history", replayToken, nil, http.StatusOK), replayManga, replayTitle, added, added, 0, 3)
	noContent(r.step("When the phone uploads the empty large cover through the double-slash path", http.MethodPost, "//resource/history", replayToken,
		historyPackage(progressed, historyRow(replayManga, replayTitle, added, added, 0, 3, "")), http.StatusNoContent))
	checkHistory(r.step("Then a double-slash pull exposes the same empty large cover", http.MethodGet, "//resource/history", replayToken, nil, http.StatusOK), replayManga, replayTitle, added, added, 0, 3)
	card := storyObject(r, r.step("And the double-slash catalog path exposes that wire value", http.MethodGet, "//manga/920003", "", nil, http.StatusOK))
	storyNumber(r, card, "manga_id", replayManga)
	cover, exists := card["large_cover_url"]
	r.require(exists && cover == "", "catalog large cover should be an empty string")

	const offlineHistoryManga int64 = 920004
	const offlineHistoryTitle = "Offline history manga"
	r.story("history_offline_over_four_days")
	offlineToken := authenticate("contract-offline@example.invalid")
	noContent(r.step("Given a phone uploads history before another goes offline", http.MethodPost, "/resource/history", offlineToken,
		historyPackage(added, historyRow(offlineHistoryManga, offlineHistoryTitle, added, added, 0, 3, "")), http.StatusNoContent))
	noContent(r.step("And the first phone advances and deletes that history", http.MethodPost, "/resource/history", offlineToken,
		historyPackage(deleted, historyRow(offlineHistoryManga, offlineHistoryTitle, added, progressed, deleted, 8, "")), http.StatusNoContent))
	r.require(afterOffline-deleted > 4*24*60*60*1000, "offline interval must exceed four days")
	checkHistory(r.step("When the offline phone uploads strictly older progress more than four days later", http.MethodPost, "/resource/history", offlineToken,
		historyPackage(afterOffline, historyRow(offlineHistoryManga, offlineHistoryTitle, added, added, 0, 3, "")), http.StatusOK), offlineHistoryManga, offlineHistoryTitle, added, progressed, deleted, 8)
	checkHistory(r.step("Then the old tombstone is still persisted", http.MethodGet, "/resource/history", offlineToken, nil, http.StatusOK), offlineHistoryManga, offlineHistoryTitle, added, progressed, deleted, 8)
	noContent(r.step("When the first phone later reads that manga again", http.MethodPost, "/resource/history", offlineToken,
		historyPackage(readded, historyRow(offlineHistoryManga, offlineHistoryTitle, added, readded, 0, 9, "")), http.StatusNoContent))
	checkHistory(r.step("Then a stale tombstone upload receives the newer reading progress", http.MethodPost, "/resource/history", offlineToken,
		historyPackage(readded, historyRow(offlineHistoryManga, offlineHistoryTitle, added, progressed, deleted, 8, "")), http.StatusOK), offlineHistoryManga, offlineHistoryTitle, added, readded, 0, 9)
	checkHistory(r.step("And the newer reading progress remains persisted", http.MethodGet, "/resource/history", offlineToken, nil, http.StatusOK), offlineHistoryManga, offlineHistoryTitle, added, readded, 0, 9)

	offline := libraryEntry{920005, 9204, "Offline", added, 0}
	removed := libraryEntry{offline.mangaID, offline.categoryID, offline.label, added, deleted}
	restored := libraryEntry{offline.mangaID, offline.categoryID, offline.label, readded, 0}
	r.story("favourites_offline_over_four_days")
	checkEmpty(r, r.step("Given the first phone preflights an empty library", http.MethodGet, "/resource/favourites", offlineToken, nil, http.StatusOK), "categories", "favourites")
	noContent(r.step("And uploads a category and favourite before the second phone goes offline", http.MethodPost, "/resource/favourites", offlineToken,
		libraryOf(added, offline), http.StatusNoContent))
	checkLibrary(r.step("When the first phone preflights before deleting", http.MethodGet, "/resource/favourites", offlineToken, nil, http.StatusOK), offline)
	noContent(r.step("And deletes both rows", http.MethodPost, "/resource/favourites", offlineToken,
		libraryOf(deleted, removed), http.StatusNoContent))
	preflight := r.step("Then the returning phone preflights and sees both old tombstones", http.MethodGet, "/resource/favourites", offlineToken, nil, http.StatusOK)
	checkLibrary(preflight, removed)
	// Model Android's preflight for keys already present in the stale local
	// snapshot: take the newer remote deletion before sending those keys.
	localCategory, localFavourite := category(offline), favourite(offline)
	remoteCategory, remoteFavourite := storyRow(r, preflight, "categories"), storyRow(r, preflight, "favourites")
	localCategory["deleted_at"] = remoteCategory["deleted_at"]
	localFavourite["deleted_at"] = remoteFavourite["deleted_at"]
	noContent(r.step("When the phone uploads its pre-applied tombstones after five offline days", http.MethodPost, "/resource/favourites", offlineToken,
		libraryPackage(afterOffline, []any{localCategory}, []any{localFavourite}), http.StatusNoContent))
	checkLibrary(r.step("Then both old tombstones remain persisted", http.MethodGet, "/resource/favourites", offlineToken, nil, http.StatusOK), removed)
	noContent(r.step("When the first phone re-adds both rows later", http.MethodPost, "/resource/favourites", offlineToken,
		libraryOf(readded, restored), http.StatusNoContent))
	preflight = r.step("Then the other phone preflights the newer re-add", http.MethodGet, "/resource/favourites", offlineToken, nil, http.StatusOK)
	checkLibrary(preflight, restored)
	remoteCategory, remoteFavourite = storyRow(r, preflight, "categories"), storyRow(r, preflight, "favourites")
	localCategory["created_at"], localCategory["deleted_at"] = remoteCategory["created_at"], remoteCategory["deleted_at"]
	localFavourite["created_at"], localFavourite["deleted_at"] = remoteFavourite["created_at"], remoteFavourite["deleted_at"]
	noContent(r.step("When the phone uploads its pre-applied re-add", http.MethodPost, "/resource/favourites", offlineToken,
		libraryPackage(readded, []any{localCategory}, []any{localFavourite}), http.StatusNoContent))
	checkLibrary(r.step("Then both restored rows remain persisted", http.MethodGet, "/resource/favourites", offlineToken, nil, http.StatusOK), restored)
}
