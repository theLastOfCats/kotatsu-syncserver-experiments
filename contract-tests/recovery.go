package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type recoveryMailAddress struct {
	Address string `json:"Address"`
}

type recoveryMailSummary struct {
	ID string                `json:"ID"`
	To []recoveryMailAddress `json:"To"`
}

type recoveryMailMessage struct {
	ID   string                `json:"ID"`
	To   []recoveryMailAddress `json:"To"`
	Text string                `json:"Text"`
}

func recoveryMailTo(addresses []recoveryMailAddress, email string) bool {
	for _, address := range addresses {
		if strings.EqualFold(address.Address, email) {
			return true
		}
	}
	return false
}

// Mailpit calls are fixture setup, not contract steps; neither their bodies nor
// the message contents belong in snapshots or failure diagnostics.
func (r *run) recoveryMailpitGet(ctx context.Context, path, operation string, result any) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.mailpitURL+path, nil)
	r.require(err == nil, "cannot construct Mailpit %s request", operation)
	response, err := r.client.Do(request)
	r.require(err == nil, "Mailpit %s request failed; check --mailpit-url and the disposable mailbox", operation)
	defer response.Body.Close()
	r.require(response.StatusCode == http.StatusOK, "Mailpit %s returned HTTP %d; expected 200", operation, response.StatusCode)
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	r.require(decoder.Decode(result) == nil, "Mailpit %s returned invalid JSON", operation)
	var trailing any
	r.require(decoder.Decode(&trailing) == io.EOF, "Mailpit %s returned trailing JSON data", operation)
}

func (r *run) recoveryMailpitMessages(ctx context.Context) []recoveryMailSummary {
	const limit = 100
	var page struct {
		Total    *uint64               `json:"total"`
		Messages []recoveryMailSummary `json:"messages"`
	}
	r.recoveryMailpitGet(ctx, "/api/v1/messages?limit=100", "message list", &page)
	r.require(page.Total != nil, "Mailpit message list is missing its total")
	r.require(*page.Total <= limit, "Mailpit mailbox contains more than %d messages; use an empty disposable mailbox", limit)
	return page.Messages
}

// Take a recipient-specific baseline before requesting mail, so a previous run's
// message can never be mistaken for the new token.
func (r *run) recoveryMailpitSeen(email string) map[string]struct{} {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen := make(map[string]struct{})
	for _, message := range r.recoveryMailpitMessages(ctx) {
		if recoveryMailTo(message.To, email) {
			r.require(message.ID != "", "Mailpit returned a message without an ID")
			seen[message.ID] = struct{}{}
		}
	}
	return seen
}

// Poll only the disposable mailbox and fetch message bodies solely for the
// requested recipient. The deadline covers both waiting and Mailpit requests.
func (r *run) recoveryMailpitToken(email string, seen map[string]struct{}) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		for _, summary := range r.recoveryMailpitMessages(ctx) {
			if !recoveryMailTo(summary.To, email) {
				continue
			}
			r.require(summary.ID != "", "Mailpit returned a message without an ID")
			if _, exists := seen[summary.ID]; exists {
				continue
			}
			seen[summary.ID] = struct{}{}
			var message recoveryMailMessage
			r.recoveryMailpitGet(ctx, "/api/v1/message/"+url.PathEscape(summary.ID), "message", &message)
			if message.Text != "" {
				r.secrets = append(r.secrets, message.Text)
			}
			r.require(message.ID == summary.ID && recoveryMailTo(message.To, email), "Mailpit message did not match its recipient or ID")
			for _, field := range strings.Fields(message.Text) {
				link := strings.Trim(field, "<>()[]{}\"'.,;")
				parsed, err := url.Parse(link)
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
					continue
				}
				if token := parsed.Query().Get("token"); token != "" {
					r.secrets = append(r.secrets, link, token)
					return token
				}
			}
			r.require(false, "Mailpit reset email has no HTTP reset link with a token query parameter")
		}
		select {
		case <-ctx.Done():
			r.require(false, "timed out waiting for the recipient's reset email; check backend SMTP delivery and --mailpit-url")
		case <-ticker.C:
		}
	}
}

func recoveryScenarios(r *run) {
	const email = "contract-recovery@example.invalid"
	const oldPassword = "recovery-old-password"
	const wrongPassword = "recovery-wrong-password"
	const newPassword = "recovery-new-password"

	r.story("password_recovery")
	auth := storyObject(r, r.step("Given a reader creates an account", http.MethodPost, "/auth", "", map[string]any{
		"email": email, "password": oldPassword,
	}, http.StatusOK))
	originalToken, ok := auth["token"].(string)
	r.require(ok && originalToken != "", "authentication must return a nonempty token")
	wrong := r.step("When the reader supplies the wrong password", http.MethodPost, "/auth", "", map[string]any{
		"email": email, "password": wrongPassword,
	}, http.StatusBadRequest)
	r.require(wrong == "Wrong password", "wrong password response differs")

	seen := r.recoveryMailpitSeen(email)
	forgot := r.step("When the reader requests password recovery", http.MethodPost, "/forgot-password", "", map[string]any{
		"email": email,
	}, http.StatusOK)
	r.require(forgot == "A password reset email was sent", "password recovery response differs")
	resetToken := r.recoveryMailpitToken(email, seen)
	reset := r.step("And the reader changes password using the delivered token", http.MethodPost, "/reset-password", "", map[string]any{
		"reset_token": resetToken, "password": newPassword,
	}, http.StatusOK)
	r.require(reset == "Password has been reset successfully", "successful password reset response differs")
	reused := r.step("Then the same reset token cannot be used again", http.MethodPost, "/reset-password", "", map[string]any{
		"reset_token": resetToken, "password": oldPassword,
	}, http.StatusBadRequest)
	r.require(reused == "Invalid or expired token", "reused token response differs")
	oldLogin := r.step("And the previous password no longer works", http.MethodPost, "/auth", "", map[string]any{
		"email": email, "password": oldPassword,
	}, http.StatusBadRequest)
	r.require(oldLogin == "Wrong password", "previous password response differs")
	newAuth := storyObject(r, r.step("And the new password signs the reader in", http.MethodPost, "/auth", "", map[string]any{
		"email": email, "password": newPassword,
	}, http.StatusOK))
	newToken, ok := newAuth["token"].(string)
	r.require(ok && newToken != "", "authentication after reset must return a nonempty token")
	checkEmpty(r, r.step("And the new session can read the account's history", http.MethodGet, "/resource/history", newToken, nil, http.StatusOK), "history")
}

// Reference-only observations; a replacement need not preserve old sessions.
func recoveryObservations(r *run) {
	const email = "contract-observation-recovery@example.invalid"
	const oldPassword = "recovery-old-password"
	const newPassword = "recovery-new-password"

	r.story("observation_password_reset_expired_token")
	auth := storyObject(r, r.step("Given a reader creates an account", http.MethodPost, "/auth", "", map[string]any{
		"email": email, "password": oldPassword,
	}, http.StatusOK))
	originalToken, ok := auth["token"].(string)
	r.require(ok && originalToken != "", "authentication must return a nonempty token")
	seen := r.recoveryMailpitSeen(email)
	forgot := r.step("When the reader requests a reset email", http.MethodPost, "/forgot-password", "", map[string]any{
		"email": email,
	}, http.StatusOK)
	r.require(forgot == "A password reset email was sent", "password recovery response differs")
	expiredToken := r.recoveryMailpitToken(email, seen)
	r.expireReset(email)
	expired := r.step("Then the delivered token is rejected after its deadline", http.MethodPost, "/reset-password", "", map[string]any{
		"reset_token": expiredToken, "password": newPassword,
	}, http.StatusBadRequest)
	r.require(expired == "Invalid or expired token", "expired token response differs")

	r.story("observation_password_reset_preserves_existing_session")
	forgot = r.step("Given the reader requests a replacement reset email", http.MethodPost, "/forgot-password", "", map[string]any{
		"email": email,
	}, http.StatusOK)
	r.require(forgot == "A password reset email was sent", "replacement recovery response differs")
	newResetToken := r.recoveryMailpitToken(email, seen)
	r.require(newResetToken != expiredToken, "replacement email must contain a new reset token")
	reset := r.step("When the reader resets the password with the replacement token", http.MethodPost, "/reset-password", "", map[string]any{
		"reset_token": newResetToken, "password": newPassword,
	}, http.StatusOK)
	r.require(reset == "Password has been reset successfully", "successful password reset response differs")
	checkEmpty(r, r.step("Then the original JWT still permits access to the reader's history", http.MethodGet, "/resource/history", originalToken, nil, http.StatusOK), "history")
}
