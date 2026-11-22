package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

type app struct {
	db               *sql.DB
	dialect          string
	jwtSecret        string
	baseURL          string
	allowNewRegister bool
	mailProvider     string
	smtpHost         string
	smtpPort         string
	smtpUsername     string
	smtpPassword     string
	smtpFrom         string
	authLimit        authLimiter
	apiLimit         authLimiter
	authPerMinute    int
	forgotPerWindow  int
	resetPerWindow   int
	passwordGate     chan struct{}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func respondJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func respondError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message))
}

func openDatabase(dialect string) (*sql.DB, error) {
	switch dialect {
	case "sqlite":
		path, err := filepath.Abs(env("DATABASE_PATH", "kotatsu.db"))
		if err != nil {
			return nil, err
		}
		u := url.URL{Scheme: "file", Path: path}
		query := u.Query()
		query.Add("_pragma", "busy_timeout(10000)")
		query.Add("_pragma", "journal_mode(WAL)")
		query.Add("_pragma", "synchronous(FULL)")
		query.Add("_pragma", "foreign_keys(1)")
		u.RawQuery = query.Encode()
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return nil, err
		}
		// Every SQLite transaction uses the same connection, avoiding deferred
		// transaction upgrade races while WAL still supports external readers.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		return db, nil
	case "mysql":
		cfg := mysql.NewConfig()
		cfg.Net = "tcp"
		cfg.Addr = net.JoinHostPort(env("DATABASE_HOST", "127.0.0.1"), env("DATABASE_PORT", "3306"))
		cfg.DBName = env("DATABASE_NAME", "kotatsu_db")
		cfg.User = os.Getenv("DATABASE_USER")
		cfg.Passwd = os.Getenv("DATABASE_PASSWORD")
		cfg.Params = map[string]string{"charset": "utf8mb4"}
		db, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(32)
		db.SetMaxIdleConns(8)
		db.SetConnMaxLifetime(5 * time.Minute)
		return db, nil
	default:
		return nil, fmt.Errorf("DATABASE_DIALECT must be sqlite or mysql")
	}
}

var resetPage = template.Must(template.New("reset").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>Reset password</title>
    <style>
        body {
            font-family:-apple-system,'Segoe UI',Roboto,Arial,sans-serif;
        }
    </style>
    <script type="text/javascript">
        window.location.href = {{.}};
    </script>
</head>
<body>
<main>
    <p>If you are not redirected automatically, <a href="{{.}}">click here</a>.</p>
</main>
</body>
</html>`))

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		respondError(w, http.StatusOK, "Alive")
	})
	mux.HandleFunc("GET /deeplink/reset-password", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if strings.TrimSpace(token) == "" {
			respondError(w, http.StatusBadRequest, "Missing token")
			return
		}
		query := url.Values{"base_url": {a.baseURL}, "token": {token}}
		link := "kotatsu://reset-password?" + query.Encode()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resetPage.Execute(w, template.URL(link))
	})
	a.registerAuth(mux)
	a.registerManga(mux)
	a.registerSync(mux)
	return withCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Some clients concatenate a trailing slash with an absolute API path.
		// Normalize before ServeMux can redirect POST requests.
		if strings.HasPrefix(r.URL.Path, "//") {
			r.URL.Path = "/" + strings.TrimLeft(r.URL.Path, "/")
			r.URL.RawPath = ""
		}
		_, pattern := mux.Handler(r)
		if pattern != "" && pattern != "GET /{$}" && pattern != "GET /deeplink/reset-password" {
			if wait := a.apiLimit.take("api", time.Second, a.apiLimit.limit); wait > 0 {
				rejectRateLimit(w, wait)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
}

func main() {
	dialect := strings.ToLower(env("DATABASE_DIALECT", "sqlite"))
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		log.Fatal("JWT_SECRET is required")
	}
	baseURL := env("BASE_URL", "http://localhost:"+env("PORT", "8080"))
	parsedBase, err := url.Parse(baseURL)
	if err != nil || (parsedBase.Scheme != "http" && parsedBase.Scheme != "https") || parsedBase.Host == "" || parsedBase.User != nil {
		log.Fatal("BASE_URL must be an absolute HTTP(S) URL")
	}
	allowed, err := strconv.ParseBool(env("ALLOW_NEW_REGISTER", "true"))
	if err != nil {
		log.Fatal("ALLOW_NEW_REGISTER must be true or false")
	}
	db, err := openDatabase(dialect)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatal(err)
	}
	a := &app{db: db, dialect: dialect, jwtSecret: secret, baseURL: strings.TrimRight(baseURL, "/"), allowNewRegister: allowed,
		mailProvider: env("MAIL_PROVIDER", "console"), smtpHost: env("SMTP_HOST", "127.0.0.1"), smtpPort: env("SMTP_PORT", "1025"),
		smtpUsername: os.Getenv("SMTP_USERNAME"), smtpPassword: os.Getenv("SMTP_PASSWORD"), smtpFrom: env("SMTP_FROM", "kotatsu@localhost")}
	if a.mailProvider != "console" && a.mailProvider != "smtp" {
		log.Fatal("MAIL_PROVIDER must be console or smtp")
	}
	for _, init := range []func() error{a.initAuth, a.initManga, a.initSync} {
		if err := init(); err != nil {
			log.Fatal(err)
		}
	}
	server := &http.Server{Addr: net.JoinHostPort(env("BIND_HOST", "127.0.0.1"), env("PORT", "8080")), Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}
