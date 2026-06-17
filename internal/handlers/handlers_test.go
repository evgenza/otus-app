package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	New().ServeHTTP(rec, req)
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
	rec := doGet(t, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	if got := decode(t, rec)["status"]; got != "работает" {
		t.Fatalf("ожидался статус \"работает\", получен %q", got)
	}
}

func TestHelloDefault(t *testing.T) {
	rec := doGet(t, "/hello")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	if got := decode(t, rec)["message"]; got != "Привет, мир!" {
		t.Fatalf("неожиданное сообщение: %q", got)
	}
}

func TestHelloWithName(t *testing.T) {
	rec := doGet(t, "/hello?name=otus")
	if got := decode(t, rec)["message"]; got != "Привет, otus!" {
		t.Fatalf("неожиданное сообщение: %q", got)
	}
}

func TestVersion(t *testing.T) {
	rec := doGet(t, "/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	body := decode(t, rec)
	for _, key := range []string{"version", "date"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("в ответе /version нет ключа %q", key)
		}
	}
}
