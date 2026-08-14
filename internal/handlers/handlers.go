package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/evgenza/otus-app/internal/audit"
	"github.com/evgenza/otus-app/internal/handlers/apidocs"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/ratelimit"
	"github.com/evgenza/otus-app/internal/security"
	"github.com/evgenza/otus-app/internal/version"
)

type Message struct {
	ID         int64     `json:"id"`
	Text       string    `json:"text"`
	Checksum   string    `json:"checksum"`
	ChecksumOK bool      `json:"checksum_ok"`
	CreatedAt  time.Time `json:"created_at"`
}

type MessageStore interface {
	Create(ctx context.Context, text, idemKey string) (Message, error)
	List(ctx context.Context) ([]Message, error)
}

type AuditLog interface {
	Record(ctx context.Context, event, text string)
	Last(ctx context.Context, n int64) ([]audit.Entry, error)
}

type MessagesCache interface {
	Get(ctx context.Context, key string) (string, bool)
	Set(ctx context.Context, key, value string)
	Delete(ctx context.Context, key string)
}

type API struct {
	store       MessageStore
	authEnabled bool
	limiter     *ratelimit.Limiter
	auditLog    AuditLog
	cache       MessagesCache
}

type Option func(*API)

func WithLimiter(l *ratelimit.Limiter) Option {
	return func(a *API) { a.limiter = l }
}

func WithAudit(log AuditLog) Option {
	return func(a *API) { a.auditLog = log }
}

func WithCache(c MessagesCache) Option {
	return func(a *API) { a.cache = c }
}

func (a *API) publicRoutes(auth *security.Auth) map[string]http.Handler {
	return map[string]http.Handler{
		"GET /health":    http.HandlerFunc(health),
		"GET /version":   http.HandlerFunc(versionInfo),
		"GET /hello":     a.limiter.Middleware(http.HandlerFunc(hello)),
		"GET /status":    http.HandlerFunc(a.statusPage),
		"GET /audit":     http.HandlerFunc(a.listAudit),
		"GET /messages":  a.limiter.Middleware(http.HandlerFunc(a.listMessages)),
		"POST /messages": auth.Middleware(a.limiter.Middleware(http.HandlerFunc(a.createMessage))),
	}
}

func New(store MessageStore, auth *security.Auth, opts ...Option) http.Handler {
	a := &API{store: store, authEnabled: auth != nil}
	for _, opt := range opts {
		opt(a)
	}
	mux := http.NewServeMux()
	for pattern, handler := range a.publicRoutes(auth) {
		mux.Handle(pattern, handler)
	}
	mux.Handle("GET /swagger/", apidocs.Handler())
	mux.Handle("GET /swagger", apidocs.Handler())
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /", hello)
	return observability.WrapHTTP("otus-app", mux)
}

func PublicRoutes() []string {
	a := &API{}
	routes := make([]string, 0)
	for pattern := range a.publicRoutes(nil) {
		routes = append(routes, pattern)
	}
	sort.Strings(routes)
	return routes
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("не удалось закодировать ответ", "err", err)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "работает"})
}

func versionInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": version.Version,
		"date":    version.Date,
	})
}

func hello(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "мир"
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Привет, " + name + "!"})
}

func (a *API) createMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректное тело запроса"})
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "поле text обязательно"})
		return
	}
	msg, err := a.store.Create(r.Context(), req.Text, r.Header.Get("Idempotency-Key"))
	if err != nil {
		slog.ErrorContext(r.Context(), "не удалось сохранить сообщение", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось сохранить сообщение"})
		return
	}
	observability.MessagesCreated.Inc()
	if a.auditLog != nil {
		a.auditLog.Record(r.Context(), "создано сообщение", msg.Text)
	}
	if a.cache != nil {
		a.cache.Delete(r.Context(), "messages")
	}
	writeJSON(w, http.StatusCreated, msg)
}

func (a *API) listMessages(w http.ResponseWriter, r *http.Request) {
	if a.cache != nil {
		if raw, ok := a.cache.Get(r.Context(), "messages"); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(raw))
			return
		}
	}
	msgs, err := a.store.List(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "не удалось получить сообщения", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось получить сообщения"})
		return
	}
	if a.cache != nil {
		if raw, err := json.Marshal(msgs); err == nil {
			a.cache.Set(r.Context(), "messages", string(raw))
		}
		w.Header().Set("X-Cache", "MISS")
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	if a.auditLog == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "аудит-лог не настроен"})
		return
	}
	entries, err := a.auditLog.Last(r.Context(), 10)
	if err != nil {
		slog.ErrorContext(r.Context(), "не удалось получить аудит-лог", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось получить аудит-лог"})
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
