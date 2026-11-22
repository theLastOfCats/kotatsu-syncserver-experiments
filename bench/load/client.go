package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type apiClient struct {
	base string
	http *http.Client
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	if u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("base URL must be HTTP without credentials, query, or fragment")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("base URL must use localhost or a loopback IP address")
	}
	return u, nil
}

func newAPIClient(base *url.URL, concurrency int) *apiClient {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// Do not use environment proxies for a benchmark containing credentials.
		Proxy:               nil,
		MaxIdleConns:        max(2, concurrency),
		MaxIdleConnsPerHost: max(2, concurrency),
		IdleConnTimeout:     90 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if strings.EqualFold(host, "localhost") {
				conn, firstErr := dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
				if firstErr == nil {
					return conn, nil
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort("::1", port))
			}
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return nil, fmt.Errorf("refusing non-loopback connection")
			}
			return dialer.DialContext(ctx, network, address)
		},
	}
	return &apiClient{
		base: strings.TrimRight(base.String(), "/"),
		http: &http.Client{
			Transport:     transport,
			Timeout:       2 * time.Minute,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *apiClient) request(ctx context.Context, method, path, token string, payload []byte, expectedStatus int) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: create request: %w", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != expectedStatus {
		// Never print response bodies: an authentication response may contain a token.
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, fmt.Errorf("%s %s: got HTTP %d, want %d", method, path, res.StatusCode, expectedStatus)
	}
	response, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if expectedStatus == http.StatusNoContent && len(response) != 0 {
		return nil, fmt.Errorf("%s %s: HTTP 204 must not have a body", method, path)
	}
	return response, nil
}

func (c *apiClient) authenticate(ctx context.Context, email, password string) (string, int64, error) {
	payload, err := json.Marshal(struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}{Email: email, Password: password})
	if err != nil {
		return "", 0, err
	}
	body, err := c.request(ctx, http.MethodPost, "/auth", "", payload, http.StatusOK)
	if err != nil {
		return "", 0, fmt.Errorf("register/authenticate (ensure registration is enabled and auth limit permits all clients): %w", err)
	}
	var auth struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &auth); err != nil || auth.Token == "" {
		return "", 0, fmt.Errorf("/auth did not return a valid token")
	}
	body, err = c.request(ctx, http.MethodGet, "/me", auth.Token, nil, http.StatusOK)
	if err != nil {
		return "", 0, err
	}
	var me struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &me); err != nil || me.ID <= 0 || me.ID > (1<<31)-1 || me.Email != email {
		return "", 0, fmt.Errorf("/me did not return the expected email and a positive 32-bit account ID")
	}
	return auth.Token, me.ID, nil
}

func (w *worker) checkEmpty(ctx context.Context) error {
	body, err := w.client.request(ctx, http.MethodGet, "/resource/history", w.token, nil, http.StatusOK)
	if err != nil {
		return err
	}
	var history syncPacket
	if err := json.Unmarshal(body, &history); err != nil || history.History == nil || len(history.History) != 0 {
		return fmt.Errorf("new account must have an empty history array")
	}
	body, err = w.client.request(ctx, http.MethodGet, "/resource/favourites", w.token, nil, http.StatusOK)
	if err != nil {
		return err
	}
	var favourites syncPacket
	if err := json.Unmarshal(body, &favourites); err != nil || favourites.Categories == nil || favourites.Favourites == nil || len(favourites.Categories) != 0 || len(favourites.Favourites) != 0 {
		return fmt.Errorf("new account must have empty category and favourite arrays")
	}
	return nil
}

func (w *worker) seed(ctx context.Context) error {
	body, err := json.Marshal(w.historyPacket())
	if err != nil {
		return err
	}
	if _, err = w.client.request(ctx, http.MethodPost, "/resource/history", w.token, body, http.StatusNoContent); err != nil {
		return err
	}
	w.lastHistoryBody = body
	body, err = json.Marshal(w.favouritesPacket())
	if err != nil {
		return err
	}
	_, err = w.client.request(ctx, http.MethodPost, "/resource/favourites", w.token, body, http.StatusNoContent)
	return err
}

// An empty device must receive the complete persisted state in a 200 response.
func (w *worker) verifyPersisted(ctx context.Context) error {
	body, err := json.Marshal(syncPacket{History: []historyRow{}, Timestamp: w.clock})
	if err != nil {
		return err
	}
	body, err = w.client.request(ctx, http.MethodPost, "/resource/history", w.token, body, http.StatusOK)
	if err != nil {
		return err
	}
	if err := w.checkHistory(body); err != nil {
		return err
	}
	body, err = json.Marshal(syncPacket{Categories: []category{}, Favourites: []favourite{}, Timestamp: w.clock})
	if err != nil {
		return err
	}
	body, err = w.client.request(ctx, http.MethodPost, "/resource/favourites", w.token, body, http.StatusOK)
	if err != nil {
		return err
	}
	return w.checkFavourites(body)
}

func (w *worker) changeHistory(ctx context.Context) error {
	i := w.historyTurn % len(w.history)
	w.historyTurn++
	w.clock++
	row := &w.history[i]
	row.UpdatedAt = w.clock
	if row.Page == 1 {
		row.Page = 2
	} else {
		row.Page = 1
	}
	body, err := json.Marshal(w.historyPacket())
	if err != nil {
		return err
	}
	if _, err := w.client.request(ctx, http.MethodPost, "/resource/history", w.token, body, http.StatusNoContent); err != nil {
		return err
	}
	w.lastHistoryBody = body
	return nil
}

func (w *worker) replayHistory(ctx context.Context) error {
	_, err := w.client.request(ctx, http.MethodPost, "/resource/history", w.token, w.lastHistoryBody, http.StatusNoContent)
	return err
}

func (w *worker) exchangeFavourites(ctx context.Context) error {
	body, err := w.client.request(ctx, http.MethodGet, "/resource/favourites", w.token, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := w.checkFavourites(body); err != nil {
		return err
	}
	i := w.favouriteTurn % len(w.favourites)
	w.favouriteTurn++
	w.clock++
	row := &w.favourites[i]
	if row.SortKey == i*2 {
		row.SortKey++
	} else {
		row.SortKey--
	}
	body, err = json.Marshal(w.favouritesPacket())
	if err != nil {
		return err
	}
	_, err = w.client.request(ctx, http.MethodPost, "/resource/favourites", w.token, body, http.StatusNoContent)
	return err
}
