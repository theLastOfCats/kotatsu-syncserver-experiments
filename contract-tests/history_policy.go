package main

import (
	"encoding/json"
	"net/http"
)

// This is a candidate acceptance policy, not a snapshot of the reference server:
// the reference revives an equally updated active row after a deletion.
func historyPolicy(r *run) {
	const created int64 = 1600000000000
	const updated int64 = created + 1000
	const deleted int64 = created + 2000
	const restored int64 = created + 3000

	for _, tc := range []struct {
		name        string
		email       string
		mangaID     int64
		deleteFirst bool
	}{
		{"history_policy_active_then_delete", "contract-policy-active@example.invalid", 930001, false},
		{"history_policy_delete_then_active", "contract-policy-delete@example.invalid", 930002, true},
	} {
		r.story(tc.name)
		auth := storyObject(r, r.step("Given a reader signs in", http.MethodPost, "/auth", "", map[string]any{
			"email": tc.email, "password": "contract-password",
		}, http.StatusOK))
		token, ok := auth["token"].(string)
		r.require(ok && token != "", "authentication must return a nonempty token")

		manga := map[string]any{
			"manga_id": tc.mangaID, "title": "History policy manga", "alt_title": nil,
			"url": "/manga/history-policy", "public_url": "https://example.invalid/manga/history-policy",
			"rating": 0.5, "content_rating": nil, "cover_url": "https://example.invalid/cover.jpg",
			"large_cover_url": "", "tags": []any{}, "state": nil, "author": nil,
			"source": "EXAMPLE", "nsfw": false,
		}
		row := func(update, removal int64, page int) map[string]any {
			return map[string]any{
				"manga_id": tc.mangaID, "manga": manga, "created_at": created,
				"updated_at": update, "chapter_id": 77, "page": page, "scroll": 0.5,
				"percent": 0.25, "chapters": 12, "deleted_at": removal,
			}
		}
		packet := func(rows ...any) map[string]any {
			return map[string]any{"history": rows, "categories": nil, "favourites": nil, "timestamp": restored}
		}
		checkRow := func(body any, expected map[string]any) {
			actual := storyRow(r, body, "history")
			got, err := json.Marshal(actual)
			r.require(err == nil, "cannot encode actual history row")
			want, err := json.Marshal(expected)
			r.require(err == nil, "cannot encode expected history row")
			r.require(string(got) == string(want), "history row differs: got %s, want %s", got, want)
		}
		emptyPull := func(name string, expected map[string]any) {
			checkRow(r.step(name, http.MethodPost, "/resource/history", token, packet([]any{}...), http.StatusOK), expected)
		}
		active := row(updated, 0, 4)
		tombstone := row(updated, deleted, 4)
		newReading := row(restored, 0, 9)
		if tc.deleteFirst {
			r.step("When the tombstone arrives first", http.MethodPost, "/resource/history", token, packet(tombstone), http.StatusNoContent)
			checkRow(r.step("Then the equally updated active row loses", http.MethodPost, "/resource/history", token, packet(active), http.StatusOK), tombstone)
		} else {
			r.step("When the active row arrives first", http.MethodPost, "/resource/history", token, packet(active), http.StatusNoContent)
			r.step("Then the equally updated tombstone wins", http.MethodPost, "/resource/history", token, packet(tombstone), http.StatusNoContent)
		}
		emptyPull("And an empty phone receives the persisted tombstone", tombstone)
		checkRow(r.step("When the offline active row is replayed", http.MethodPost, "/resource/history", token, packet(active), http.StatusOK), tombstone)
		r.step("And the tombstone is replayed", http.MethodPost, "/resource/history", token, packet(tombstone), http.StatusNoContent)
		emptyPull("Then the persisted row is still deleted", tombstone)

		// The new reading is later than both the old update and deletion, avoiding
		// imposing an ordering for clocks skewed between those two events.
		r.step("When reading resumes after the deletion", http.MethodPost, "/resource/history", token, packet(newReading), http.StatusNoContent)
		emptyPull("Then an empty phone receives the restored reading", newReading)
		checkRow(r.step("When the old tombstone is replayed after restoration", http.MethodPost, "/resource/history", token, packet(tombstone), http.StatusOK), newReading)
		emptyPull("Then the restored reading remains persisted", newReading)
	}
}
