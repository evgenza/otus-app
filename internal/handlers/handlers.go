package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/evgenza/otus-app/internal/handlers/apidocs"
	"github.com/evgenza/otus-app/internal/observability"
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
	Create(ctx context.Context, text string) (Message, error)
	List(ctx context.Context) ([]Message, error)
}

type API struct {
	store       MessageStore
	authEnabled bool
}

func New(store MessageStore, auth *security.Auth) http.Handler {
	a := &API{store: store, authEnabled: auth != nil}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /version", versionInfo)
	mux.HandleFunc("GET /hello", hello)
	mux.HandleFunc("GET /status", a.statusPage)
	mux.Handle("GET /swagger/", apidocs.Handler())
	mux.Handle("GET /swagger", apidocs.Handler())
	mux.Handle("POST /messages", auth.Middleware(http.HandlerFunc(a.createMessage)))
	mux.HandleFunc("GET /messages", a.listMessages)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /", hello)
	return observability.WrapHTTP("otus-app", mux)
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
	msg, err := a.store.Create(r.Context(), req.Text)
	if err != nil {
		slog.ErrorContext(r.Context(), "не удалось сохранить сообщение", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось сохранить сообщение"})
		return
	}
	observability.MessagesCreated.Inc()
	writeJSON(w, http.StatusCreated, msg)
}

func (a *API) listMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := a.store.List(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "не удалось получить сообщения", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось получить сообщения"})
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}
