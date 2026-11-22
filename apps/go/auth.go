package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"
)

const (
	issuer         = "http://0.0.0.0:8080/"
	audience       = "http://0.0.0.0:8080/resource"
	passwordLength = "Password should be from 2 to 24 characters long"
)

type sessionClaims struct {
	UserID  int64 `json:"user_id"`
	Version int64 `json:"version"`
	jwt.RegisteredClaims
}

type authWindow struct {
	deadline time.Time
	used     int
}
type authLimiter struct {
	mu          sync.Mutex
	entries     map[string]authWindow
	limit       int
	nextCleanup time.Time
}

func (a *app) initAuth() error {
	if len(a.jwtSecret) < 32 {
		return errors.New("set JWT_SECRET to at least 32 random bytes")
	}
	passwordConcurrency := 2
	settings := []struct {
		name string
		def  int
		dest *int
	}{
		{"RATE_LIMIT_API_PER_SECOND", 200, &a.apiLimit.limit},
		{"RATE_LIMIT_AUTH_PER_MINUTE", 5, &a.authPerMinute},
		{"RATE_LIMIT_FORGOT_PER_10_MINUTES", 5, &a.forgotPerWindow},
		{"RATE_LIMIT_RESET_PER_10_MINUTES", 5, &a.resetPerWindow},
		{"RATE_LIMIT_PASSWORD_CONCURRENCY", 2, &passwordConcurrency},
	}
	for _, setting := range settings {
		*setting.dest = setting.def
		if raw, present := os.LookupEnv(setting.name); present {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return fmt.Errorf("invalid %s", setting.name)
			}
			*setting.dest = n
		}
	}
	a.passwordGate = make(chan struct{}, passwordConcurrency)
	a.apiLimit.entries = make(map[string]authWindow)
	a.authLimit.entries = make(map[string]authWindow)
	identity := "INTEGER PRIMARY KEY"
	if a.dialect == "mysql" {
		identity = "BIGINT PRIMARY KEY AUTO_INCREMENT"
	}
	_, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS users (id ` + identity + `, email VARCHAR(320) NOT NULL UNIQUE,
		password_hash VARCHAR(160) NOT NULL, session_version BIGINT NOT NULL DEFAULT 0,
		reset_hash CHAR(64) UNIQUE, reset_expires BIGINT)`)
	return err
}

func (l *authLimiter) take(key string, ttl time.Duration, limit int) time.Duration {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.nextCleanup) {
		for k, b := range l.entries {
			if !now.Before(b.deadline) {
				delete(l.entries, k)
			}
		}
		l.nextCleanup = now.Add(time.Minute)
	}
	b := l.entries[key]
	if !now.Before(b.deadline) {
		b = authWindow{deadline: now.Add(ttl)}
	}
	if b.used >= limit || (b.used == 0 && len(l.entries) >= 65536) {
		return b.deadline.Sub(now)
	}
	b.used++
	l.entries[key] = b
	return 0
}

func rejectRateLimit(w http.ResponseWriter, wait time.Duration) {
	seconds := (wait + time.Second - 1) / time.Second
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
	w.WriteHeader(http.StatusTooManyRequests)
}

func (a *app) throttled(endpoint string, ttl time.Duration, limit int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if wait := a.authLimit.take(endpoint+":"+ip, ttl, limit); wait > 0 {
			rejectRateLimit(w, wait)
			return
		}
		next(w, r)
	}
}

func (a *app) passwordWork(w http.ResponseWriter, work func()) bool {
	select {
	case a.passwordGate <- struct{}{}:
		defer func() { <-a.passwordGate }()
		work()
		return true
	default:
		w.Header().Set("Retry-After", "1")
		authText(w, http.StatusServiceUnavailable, "Password service is busy")
		return false
	}
}

func (a *app) registerAuth(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth", a.throttled("auth", time.Minute, a.authPerMinute, a.signIn))
	mux.HandleFunc("POST /forgot-password", a.throttled("forgot", 10*time.Minute, a.forgotPerWindow, a.forgot))
	mux.HandleFunc("POST /reset-password", a.throttled("reset", 10*time.Minute, a.resetPerWindow, a.reset))
	mux.HandleFunc("GET /me", a.profile)
}

func authText(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, message)
}
func authDBError(w http.ResponseWriter) { authText(w, 500, "Internal server error") }
func readAuth(w http.ResponseWriter, r *http.Request, dest any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(dest); err != nil {
		authText(w, 400, "Invalid JSON")
		return false
	}
	return true
}
func utf16Length(s string) (n int) {
	for _, c := range s {
		n += utf16.RuneLen(c)
	}
	return
}
func validPassword(pw string) bool { n := utf16Length(pw); return n >= 2 && n <= 24 }
func newHash(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	digest := argon2.IDKey([]byte(pw), salt, 3, 65536, 4, 32)
	return "$argon2id$v=19$m=65536,t=3,p=4$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(digest)
}
func matchesHash(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=4" {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, e2 := base64.RawStdEncoding.DecodeString(parts[5])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(want) != 32 {
		return false
	}
	return subtle.ConstantTimeCompare(want, argon2.IDKey([]byte(pw), salt, 3, 65536, 4, 32)) == 1
}

func (a *app) signIn(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string
		Password string
	}
	if !readAuth(w, r, &in) {
		return
	}
	if !validPassword(in.Password) {
		authText(w, 400, "400: java.lang.IllegalArgumentException: "+passwordLength)
		return
	}
	if n := utf16Length(in.Email); n < 5 || n > 320 || !strings.Contains(in.Email, "@") || strings.ContainsAny(in.Email, "\r\n") {
		authText(w, 400, "400: java.lang.IllegalArgumentException: Invalid email address")
		return
	}
	var id, version int64
	var hash string
	lookup := func() error {
		return a.db.QueryRowContext(r.Context(), `SELECT id,password_hash,session_version FROM users WHERE email=?`, in.Email).Scan(&id, &hash, &version)
	}
	err := lookup()
	created := false
	if errors.Is(err, sql.ErrNoRows) && a.allowNewRegister {
		var passwordHash string
		if !a.passwordWork(w, func() { passwordHash = newHash(in.Password) }) {
			return
		}
		res, insertErr := a.db.ExecContext(r.Context(), `INSERT INTO users(email,password_hash) VALUES(?,?)`, in.Email, passwordHash)
		if insertErr == nil {
			id, err = res.LastInsertId()
			created = true
		} else {
			err = lookup()
			if errors.Is(err, sql.ErrNoRows) {
				err = insertErr
			}
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		authDBError(w)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		authText(w, 400, "Wrong password")
		return
	}
	if !created {
		var matches bool
		if !a.passwordWork(w, func() { matches = matchesHash(in.Password, hash) }) {
			return
		}
		if !matches {
			authText(w, 400, "Wrong password")
			return
		}
	}
	claims := sessionClaims{UserID: id, Version: version, RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Audience: jwt.ClaimStrings{audience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(30 * 24 * time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(a.jwtSecret))
	if err != nil {
		authDBError(w)
		return
	}
	respondJSON(w, 200, map[string]string{"token": token})
}

func (a *app) userID(r *http.Request) (int64, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return 0, false
	}
	claims := new(sessionClaims)
	_, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return []byte(a.jwtSecret), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired())
	if err != nil || claims.UserID <= 0 {
		return 0, false
	}
	var version int64
	err = a.db.QueryRowContext(r.Context(), `SELECT session_version FROM users WHERE id=?`, claims.UserID).Scan(&version)
	return claims.UserID, err == nil && version == claims.Version
}

func (a *app) profile(w http.ResponseWriter, r *http.Request) {
	id, ok := a.userID(r)
	if !ok {
		w.WriteHeader(401)
		return
	}
	var email string
	if err := a.db.QueryRowContext(r.Context(), `SELECT email FROM users WHERE id=?`, id).Scan(&email); err != nil {
		authDBError(w)
		return
	}
	respondJSON(w, 200, map[string]any{"id": id, "email": email, "nickname": nil})
}

func resetDigest(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
func (a *app) forgot(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string }
	if !readAuth(w, r, &in) {
		return
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	token := hex.EncodeToString(secret)
	hash := resetDigest(token)
	now := time.Now().Unix()
	res, err := a.db.ExecContext(r.Context(), `UPDATE users SET reset_hash=?,reset_expires=? WHERE email=? AND (reset_hash IS NULL OR reset_expires<=?)`, hash, now+900, in.Email, now)
	if err != nil {
		authDBError(w)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		authDBError(w)
		return
	}
	if n == 1 {
		link := strings.TrimRight(a.baseURL, "/") + "/deeplink/reset-password?token=" + url.QueryEscape(token)
		if err := a.sendReset(in.Email, link); err != nil {
			_, _ = a.db.ExecContext(r.Context(), `UPDATE users SET reset_hash=NULL,reset_expires=NULL WHERE reset_hash=?`, hash)
			authDBError(w)
			return
		}
	}
	authText(w, 200, "A password reset email was sent")
}
func (a *app) sendReset(email, link string) error {
	if a.mailProvider == "console" {
		log.Printf("[console mail] To: %s\nSubject: Password reset\nYou can reset your password at %s", email, link)
		return nil
	}
	if a.mailProvider != "smtp" {
		return errors.New("unknown mail provider")
	}
	to, err := mail.ParseAddress(email)
	if err != nil {
		return err
	}
	from, err := mail.ParseAddress(a.smtpFrom)
	if err != nil {
		return err
	}
	var auth smtp.Auth
	if a.smtpUsername != "" {
		auth = smtp.PlainAuth("", a.smtpUsername, a.smtpPassword, a.smtpHost)
	}
	msg := "From: " + from.String() + "\r\nTo: " + to.String() + "\r\nSubject: Password reset\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nYou can reset your password at " + link + "\r\n"
	return smtp.SendMail(net.JoinHostPort(a.smtpHost, a.smtpPort), auth, from.Address, []string{to.Address}, []byte(msg))
}
func (a *app) reset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"reset_token"`
		Password string
	}
	if !readAuth(w, r, &in) {
		return
	}
	hash := resetDigest(in.Token)
	var id int64
	err := a.db.QueryRowContext(r.Context(), `SELECT id FROM users WHERE reset_hash=? AND reset_expires>?`, hash, time.Now().Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		authText(w, 400, "Invalid or expired token")
		return
	}
	if err != nil {
		authDBError(w)
		return
	}
	if !validPassword(in.Password) {
		authText(w, 400, passwordLength)
		return
	}
	var passwordHash string
	if !a.passwordWork(w, func() { passwordHash = newHash(in.Password) }) {
		return
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE users SET password_hash=?,session_version=session_version+1,reset_hash=NULL,reset_expires=NULL WHERE id=? AND reset_hash=? AND reset_expires>?`, passwordHash, id, hash, time.Now().Unix())
	if err != nil {
		authDBError(w)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		authDBError(w)
		return
	}
	if n != 1 {
		authText(w, 400, "Invalid or expired token")
		return
	}
	authText(w, 200, "Password has been reset successfully")
}
