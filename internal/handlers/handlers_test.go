package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evgenza/otus-app/internal/handlers/apidocs"
	"github.com/evgenza/otus-app/internal/security"
)

type fakeStore struct {
	items   []Message
	keys    map[string]Message
	failAll bool
}

func (f *fakeStore) Create(_ context.Context, text, idemKey string) (Message, error) {
	if f.failAll {
		return Message{}, errors.New("сбой хранилища")
	}
	if idemKey != "" {
		if m, ok := f.keys[idemKey]; ok {
			return m, nil
		}
	}
	m := Message{
		ID:         int64(len(f.items) + 1),
		Text:       text,
		Checksum:   security.Checksum(text),
		ChecksumOK: true,
		CreatedAt:  time.Now(),
	}
	f.items = append(f.items, m)
	if idemKey != "" {
		if f.keys == nil {
			f.keys = make(map[string]Message)
		}
		f.keys[idemKey] = m
	}
	return m, nil
}

func (f *fakeStore) List(_ context.Context) ([]Message, error) {
	if f.failAll {
		return nil, errors.New("сбой хранилища")
	}
	return f.items, nil
}

func do(t *testing.T, store MessageStore, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	New(store, nil).ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("некорректный JSON в ответе: %v", err)
	}
	return body
}

func TestHealth(t *testing.T) {
	rec := do(t, &fakeStore{}, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	if got := decode(t, rec)["status"]; got != "работает" {
		t.Fatalf("ожидался статус \"работает\", получен %q", got)
	}
}

func TestHelloDefault(t *testing.T) {
	rec := do(t, &fakeStore{}, http.MethodGet, "/hello", "")
	if got := decode(t, rec)["message"]; got != "Привет, мир!" {
		t.Fatalf("неожиданное сообщение: %q", got)
	}
}

func TestHelloWithName(t *testing.T) {
	rec := do(t, &fakeStore{}, http.MethodGet, "/hello?name=otus", "")
	if got := decode(t, rec)["message"]; got != "Привет, otus!" {
		t.Fatalf("неожиданное сообщение: %q", got)
	}
}

func TestVersion(t *testing.T) {
	rec := do(t, &fakeStore{}, http.MethodGet, "/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	for _, key := range []string{"version", "date"} {
		if _, ok := decode(t, rec)[key]; !ok {
			t.Fatalf("в ответе /version нет ключа %q", key)
		}
	}
}

func TestCreateMessage(t *testing.T) {
	store := &fakeStore{}
	rec := do(t, store, http.MethodPost, "/messages", `{"text":"привет"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ожидался статус 201, получен %d", rec.Code)
	}
	var msg Message
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("некорректный JSON: %v", err)
	}
	if msg.ID == 0 || msg.Text != "привет" {
		t.Fatalf("неожиданное сообщение: %+v", msg)
	}
	if msg.Checksum != security.Checksum("привет") || !msg.ChecksumOK {
		t.Fatalf("контрольная сумма не заполнена: %+v", msg)
	}
	if len(store.items) != 1 {
		t.Fatalf("ожидалось 1 сообщение в хранилище, получено %d", len(store.items))
	}
}

func TestCreateMessageIdempotent(t *testing.T) {
	store := &fakeStore{}
	handler := New(store, nil)
	var ids []int64
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(`{"text":"один раз"}`))
		req.Header.Set("Idempotency-Key", "ключ-123")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("ожидался статус 201, получен %d", rec.Code)
		}
		var msg Message
		if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
			t.Fatalf("некорректный JSON: %v", err)
		}
		ids = append(ids, msg.ID)
	}
	if ids[0] != ids[1] {
		t.Fatalf("повтор с тем же ключом вернул другое сообщение: %d и %d", ids[0], ids[1])
	}
	if len(store.items) != 1 {
		t.Fatalf("ожидалось 1 сообщение в хранилище, получено %d", len(store.items))
	}
}

func TestCreateMessageEmptyText(t *testing.T) {
	rec := do(t, &fakeStore{}, http.MethodPost, "/messages", `{"text":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ожидался статус 400, получен %d", rec.Code)
	}
}

func TestListMessages(t *testing.T) {
	store := &fakeStore{}
	_, _ = store.Create(context.Background(), "первое", "")
	_, _ = store.Create(context.Background(), "второе", "")
	rec := do(t, store, http.MethodGet, "/messages", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	var msgs []Message
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("некорректный JSON: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("ожидалось 2 сообщения, получено %d", len(msgs))
	}
}

func TestListMessagesStoreError(t *testing.T) {
	rec := do(t, &fakeStore{failAll: true}, http.MethodGet, "/messages", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ожидался статус 500, получен %d", rec.Code)
	}
}

func TestStatusPage(t *testing.T) {
	store := &fakeStore{}
	_, _ = store.Create(context.Background(), "первое", "")
	rec := do(t, store, http.MethodGet, "/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("ожидался text/html, получен %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Состояние сервиса", "доступна", "все сходятся"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет фрагмента %q", want)
		}
	}
}

func TestStatusPageDBError(t *testing.T) {
	rec := do(t, &fakeStore{failAll: true}, http.MethodGet, "/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "недоступна") {
		t.Fatal("страница не показывает недоступность базы")
	}
}

func TestSwaggerUI(t *testing.T) {
	rec := do(t, &fakeStore{}, http.MethodGet, "/swagger/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "swagger-ui") {
		t.Fatal("в ответе нет страницы Swagger UI")
	}
}

func TestSpecMatchesRoutes(t *testing.T) {
	appRoutes := PublicRoutes()
	specRoutes := make([]string, 0)
	for _, route := range apidocs.SpecRoutes() {
		if !strings.Contains(route, " /gw/") {
			specRoutes = append(specRoutes, route)
		}
	}

	spec := make(map[string]bool, len(specRoutes))
	for _, route := range specRoutes {
		spec[route] = true
	}
	for _, route := range appRoutes {
		if !spec[route] {
			t.Errorf("маршрут %q зарегистрирован в приложении, но отсутствует в openapi.yaml", route)
		}
	}

	registered := make(map[string]bool, len(appRoutes))
	for _, route := range appRoutes {
		registered[route] = true
	}
	for _, route := range specRoutes {
		if !registered[route] {
			t.Errorf("маршрут %q описан в openapi.yaml, но не зарегистрирован в приложении", route)
		}
	}
}

func TestSwaggerSpec(t *testing.T) {
	rec := do(t, &fakeStore{}, http.MethodGet, "/swagger/openapi.yaml", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"openapi: 3.0", "/messages", "bearerAuth"} {
		if !strings.Contains(body, want) {
			t.Fatalf("в спецификации нет фрагмента %q", want)
		}
	}
}
