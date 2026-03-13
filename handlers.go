package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web
var webFS embed.FS

type AuthConfig struct {
	Enabled      bool
	Username     string
	PasswordHash string
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]time.Time
}

func NewSessionStore() *SessionStore {
	ss := &SessionStore{sessions: make(map[string]time.Time)}
	go ss.cleanup()
	return ss
}

func (ss *SessionStore) Create() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	ss.mu.Lock()
	ss.sessions[token] = time.Now().Add(24 * time.Hour)
	ss.mu.Unlock()
	return token
}

func (ss *SessionStore) Valid(token string) bool {
	ss.mu.RLock()
	expiry, ok := ss.sessions[token]
	ss.mu.RUnlock()
	return ok && time.Now().Before(expiry)
}

func (ss *SessionStore) Delete(token string) {
	ss.mu.Lock()
	delete(ss.sessions, token)
	ss.mu.Unlock()
}

func (ss *SessionStore) cleanup() {
	for {
		time.Sleep(10 * time.Minute)
		now := time.Now()
		ss.mu.Lock()
		for tok, exp := range ss.sessions {
			if now.After(exp) {
				delete(ss.sessions, tok)
			}
		}
		ss.mu.Unlock()
	}
}

func NewServer(store *Store, addr string, auth *AuthConfig) *http.Server {
	sessions := NewSessionStore()
	mux := http.NewServeMux()

	mux.HandleFunc("/login", handleLogin(auth, sessions))
	mux.HandleFunc("/logout", handleLogout(sessions))
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/records", handleRecords(store))
	mux.HandleFunc("/api/detail", handleDetail(store))
	mux.HandleFunc("/api/stats", handleStats(store))
	mux.HandleFunc("/api/status", handleStatus(store))

	var handler http.Handler = mux
	if auth != nil && auth.Enabled {
		handler = authMiddleware(sessions, handler)
	}
	handler = logMiddleware(handler)

	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
}

func authMiddleware(sessions *SessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || !sessions.Valid(cookie.Value) {
			if r.URL.Path == "/logout" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			if isAPIRequest(r) {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAPIRequest(r *http.Request) bool {
	return len(r.URL.Path) > 4 && r.URL.Path[:5] == "/api/"
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/status" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func handleLogin(auth *AuthConfig, sessions *SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			data, err := webFS.ReadFile("web/login.html")
			if err != nil {
				http.Error(w, "internal error", 500)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		username := r.FormValue("username")
		password := r.FormValue("password")

		if auth == nil || !auth.Enabled ||
			username != auth.Username ||
			bcrypt.CompareHashAndPassword([]byte(auth.PasswordHash), []byte(password)) != nil {
			http.Redirect(w, r, "/login?error=1", http.StatusFound)
			return
		}

		token := sessions.Create()
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func handleLogout(sessions *SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session"); err == nil {
			sessions.Delete(cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			MaxAge:   -1,
		})
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func handleRecords(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		draw, _ := strconv.Atoi(q.Get("draw"))
		start, _ := strconv.Atoi(q.Get("start"))
		length, _ := strconv.Atoi(q.Get("length"))
		if length <= 0 {
			length = 25
		}
		if length > 500 {
			length = 500
		}
		search := q.Get("search[value]")
		orderCol, _ := strconv.Atoi(q.Get("order[0][column]"))
		orderDir := q.Get("order[0][dir]")
		if orderDir != "asc" && orderDir != "desc" {
			orderDir = "desc"
		}

		result := store.Search(SearchParams{
			Draw:          draw,
			Start:         start,
			Length:        length,
			SearchTerm:    search,
			OrderCol:      orderCol,
			OrderDir:      orderDir,
			FilterFrom:    q.Get("f_from"),
			FilterTo:      q.Get("f_to"),
			FilterClient:  q.Get("f_client"),
			FilterRelay:   q.Get("f_relay"),
			FilterStatus:  q.Get("f_status"),
			FilterQueueID: q.Get("f_qid"),
			FilterTLS:     q.Get("f_tls"),
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func handleDetail(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qid := r.URL.Query().Get("qid")
		if qid == "" {
			http.Error(w, `{"error":"missing qid"}`, http.StatusBadRequest)
			return
		}
		detail := store.GetDetail(qid)
		if detail == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(detail)
	}
}

func handleStats(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := store.Stats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	}
}

func handleStatus(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ready, status, count := store.GetStatus()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ready":   ready,
			"status":  status,
			"records": count,
		})
	}
}
