package main

import (
	"encoding/json"
	"net/http"
)

// Candidate acceptance policy: these assertions are not reference snapshots.
func favouritesPolicy(r *run) {
	const created int64 = 1600000000000
	const olderDelete int64 = created + 1000
	const staleReadd int64 = created + 1500
	const newerDelete int64 = created + 2000
	const readded int64 = created + 3000

	manga := func(id int64) map[string]any {
		return map[string]any{
			"manga_id": id, "title": "Policy manga", "alt_title": nil,
			"url": "/manga/policy", "public_url": "https://example.invalid/manga/policy",
			"rating": 0.5, "content_rating": nil, "cover_url": "https://example.invalid/cover.jpg",
			"large_cover_url": "", "tags": []any{}, "state": nil, "author": nil,
			"source": "EXAMPLE", "nsfw": false,
		}
	}
	category := func(id, added, removed int64, sort int) map[string]any {
		return map[string]any{"category_id": id, "created_at": added, "sort_key": sort,
			"track": true, "title": "Policy shelf", "order": "MANUAL", "deleted_at": removed, "show_in_lib": true}
	}
	favourite := func(id, categoryID, added, removed int64, sort int) map[string]any {
		return map[string]any{"manga_id": id, "manga": manga(id), "category_id": categoryID,
			"sort_key": sort, "pinned": true, "created_at": added, "deleted_at": removed}
	}
	packet := func(categories, favourites []any) map[string]any {
		return map[string]any{"history": nil, "categories": categories, "favourites": favourites, "timestamp": readded}
	}
	authenticate := func(email string) string {
		auth := storyObject(r, r.step("Given a reader signs in", http.MethodPost, "/auth", "", map[string]any{
			"email": email, "password": "contract-password",
		}, http.StatusOK))
		token, ok := auth["token"].(string)
		r.require(ok && token != "", "authentication must return a nonempty token")
		return token
	}
	checkRow := func(body any, key string, expected map[string]any) {
		actual := storyRow(r, body, key)
		got, err := json.Marshal(actual)
		r.require(err == nil, "cannot encode actual %s row", key)
		want, err := json.Marshal(expected)
		r.require(err == nil, "cannot encode expected %s row", key)
		r.require(string(got) == string(want), "%s row differs: got %s, want %s", key, got, want)
	}

	r.story("favourites_policy_tombstone_replay")
	token := authenticate("contract-favourites-policy@example.invalid")
	cat := func(added, removed int64) map[string]any { return category(9401, added, removed, 1) }
	fav := func(added, removed int64) map[string]any { return favourite(940001, 9401, added, removed, 1) }
	library := func(added, removed int64) map[string]any {
		return packet([]any{cat(added, removed)}, []any{fav(added, removed)})
	}
	checkLibrary := func(body any, added, removed int64) {
		checkRow(body, "categories", cat(added, removed))
		checkRow(body, "favourites", fav(added, removed))
	}
	post := func(name string, body any, status int) any {
		return r.step(name, http.MethodPost, "/resource/favourites", token, body, status)
	}
	r.require(post("When the phone uploads its active library", library(created, 0), http.StatusNoContent) == nil, "204 must be bodyless")
	r.require(post("And a newer deletion arrives", library(created, newerDelete), http.StatusNoContent) == nil, "204 must be bodyless")
	checkLibrary(post("Then an older nonzero deletion loses", library(created, olderDelete), http.StatusOK), created, newerDelete)
	checkLibrary(post("And a re-add between the deletions loses", library(staleReadd, 0), http.StatusOK), created, newerDelete)
	checkLibrary(post("And an empty second phone receives both tombstones", packet([]any{}, []any{}), http.StatusOK), created, newerDelete)
	r.require(post("When both rows are genuinely re-added later", library(readded, 0), http.StatusNoContent) == nil, "204 must be bodyless")
	r.require(post("And the re-add is repeated identically", library(readded, 0), http.StatusNoContent) == nil, "204 must be bodyless")
	checkLibrary(post("Then an empty second phone receives the restored rows", packet([]any{}, []any{}), http.StatusOK), readded, 0)

	r.story("favourites_policy_partial_existing_updates")
	token = authenticate("contract-favourites-partial-policy@example.invalid")
	firstCategory := category(9411, created, 0, 1)
	secondCategory := category(9412, created, 0, 2)
	firstFavourite := favourite(941001, 9411, created, 0, 1)
	secondFavourite := favourite(941002, 9412, created, 0, 2)
	r.require(post("Given two existing categories and favourites", packet([]any{firstCategory, secondCategory}, []any{firstFavourite, secondFavourite}), http.StatusNoContent) == nil, "204 must be bodyless")
	changedCategory := category(9411, created, 0, 7)
	changedFavourite := favourite(941001, 9411, created, 0, 8)
	checkPartial := func(body any) {
		object := storyObject(r, body)
		for _, group := range []struct { key, idKey string; expected []map[string]any }{
			{"categories", "category_id", []map[string]any{changedCategory, secondCategory}},
			{"favourites", "manga_id", []map[string]any{changedFavourite, secondFavourite}},
		} {
			rows, ok := object[group.key].([]any)
			r.require(ok && len(rows) == len(group.expected), "expected two %s rows", group.key)
			for i, expected := range group.expected {
				// Array order is part of the wire contract: rows are ordered by ID.
				actual, err := json.Marshal(rows[i])
				r.require(err == nil, "cannot encode %s row", group.key)
				want, err := json.Marshal(expected)
				r.require(err == nil && string(actual) == string(want), "%s row %s differs: got %s, want %s", group.key, group.idKey, actual, want)
			}
		}
	}
	checkPartial(post("When a partial packet changes existing category and favourite sort", packet([]any{changedCategory}, []any{changedFavourite}), http.StatusOK))
	checkPartial(post("Then an empty phone sees all existing rows and updates", packet([]any{}, []any{}), http.StatusOK))
	history := func(id int64, update int64, page int) map[string]any {
		return map[string]any{"manga_id": id, "manga": manga(id), "created_at": created, "updated_at": update,
			"chapter_id": 77, "page": page, "scroll": 0.5, "percent": 0.25, "chapters": 12, "deleted_at": int64(0)}
	}
	historyPacket := func(rows ...any) map[string]any {
		if rows == nil {
			rows = []any{}
		}
		return map[string]any{"history": rows, "categories": nil, "favourites": nil, "timestamp": readded}
	}
	firstHistory := history(941001, created, 1)
	secondHistory := history(941002, created, 2)
	r.require(r.step("Given two existing history rows", http.MethodPost, "/resource/history", token, historyPacket(firstHistory, secondHistory), http.StatusNoContent) == nil, "204 must be bodyless")
	changedHistory := history(941001, newerDelete, 9)
	checkHistory := func(body any) {
		rows, ok := storyObject(r, body)["history"].([]any)
		r.require(ok && len(rows) == 2, "expected two history rows")
		for i, expected := range []map[string]any{changedHistory, secondHistory} {
			got, err := json.Marshal(rows[i])
			r.require(err == nil, "cannot encode history row")
			want, err := json.Marshal(expected)
			r.require(err == nil && string(got) == string(want), "history row differs: got %s, want %s", got, want)
		}
	}
	checkHistory(r.step("When a partial packet advances an existing page", http.MethodPost, "/resource/history", token, historyPacket(changedHistory), http.StatusOK))
	checkHistory(r.step("Then an empty phone sees both history rows", http.MethodPost, "/resource/history", token, historyPacket(), http.StatusOK))

	r.story("favourites_policy_shared_tag")
	firstToken := authenticate("contract-favourites-tag-first@example.invalid")
	tag := func(title string) map[string]any {
		return map[string]any{"tag_id": 94201, "title": title, "key": "policy", "source": "EXAMPLE", "pinned": false}
	}
	tagged := func(id, categoryID int64, title string) map[string]any {
		row := favourite(id, categoryID, created, 0, 1)
		row["manga"].(map[string]any)["tags"] = []any{tag(title)}
		return row
	}
	firstTagCategory := category(9421, created, 0, 1)
	firstTagged := tagged(942001, 9421, "Original")
	r.require(r.step("Given a reader stores a tagged manga", http.MethodPost, "/resource/favourites", firstToken,
		packet([]any{firstTagCategory}, []any{firstTagged}), http.StatusNoContent) == nil, "204 must be bodyless")
	secondToken := authenticate("contract-favourites-tag-second@example.invalid")
	r.require(r.step("When another reader publishes the same tag with a new title", http.MethodPost, "/resource/favourites", secondToken,
		packet([]any{category(9422, created, 0, 1)}, []any{tagged(942002, 9422, "Changed")}), http.StatusNoContent) == nil, "204 must be bodyless")
	shared := r.step("Then the first reader receives the shared updated tag", http.MethodPost, "/resource/favourites", firstToken,
		packet([]any{}, []any{}), http.StatusOK)
	checkRow(shared, "categories", firstTagCategory)
	checkRow(shared, "favourites", tagged(942001, 9421, "Changed"))
}
