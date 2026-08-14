package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evgenza/otus-app/internal/handlers/apidocs"
)

func TestGatewayProxiesCreate(t *testing.T) {
	var gotMethod, gotPath, gotContentType, gotForwardedFor string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}))
	defer backend.Close()

	g := &gateway{appURL: backend.URL, client: backend.Client()}
	srv := httptest.NewServer(g.routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/gw/messages", "application/json", strings.NewReader(`{"text":"привет"}`))
	if err != nil {
		t.Fatalf("запрос к gateway не удался: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ожидался статус 201, получен %d", resp.StatusCode)
	}
	if gotMethod != http.MethodPost || gotPath != "/messages" {
		t.Errorf("ожидался POST /messages, получен %s %s", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type не проброшен в app: %q", gotContentType)
	}
	if gotForwardedFor == "" {
		t.Error("не проставлен заголовок X-Forwarded-For")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "привет") {
		t.Errorf("тело ответа не проброшено обратно: %s", body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type ответа не проброшен: %q", got)
	}
}

func TestGatewayProxiesList(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"text":"привет"}]`))
	}))
	defer backend.Close()

	g := &gateway{appURL: backend.URL, client: backend.Client()}
	srv := httptest.NewServer(g.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/gw/messages")
	if err != nil {
		t.Fatalf("запрос к gateway не удался: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", resp.StatusCode)
	}
	var msgs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatalf("ответ не является корректным JSON: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("ожидалось 1 сообщение, получено %d", len(msgs))
	}
}

func TestGatewayBackendDown(t *testing.T) {
	g := &gateway{appURL: "http://127.0.0.1:1", client: &http.Client{Timeout: time.Second}}
	srv := httptest.NewServer(g.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/gw/messages")
	if err != nil {
		t.Fatalf("запрос к gateway не удался: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("ожидался статус 502, получен %d", resp.StatusCode)
	}
	var e map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("ответ не является корректным JSON: %v", err)
	}
	if e["error"] != "app недоступен" {
		t.Errorf("неожиданный текст ошибки: %q", e["error"])
	}
}

func TestGatewayAddsIdempotencyKey(t *testing.T) {
	var gotKeys []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeys = append(gotKeys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	g := &gateway{appURL: backend.URL, client: backend.Client()}
	srv := httptest.NewServer(g.routes())
	defer srv.Close()

	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/gw/messages", "application/json", strings.NewReader(`{"text":"x"}`))
		if err != nil {
			t.Fatalf("запрос к gateway не удался: %v", err)
		}
		_ = resp.Body.Close()
	}
	if len(gotKeys) != 2 || gotKeys[0] == "" || gotKeys[1] == "" {
		t.Fatalf("gateway не проставил ключ идемпотентности: %v", gotKeys)
	}
	if gotKeys[0] == gotKeys[1] {
		t.Fatal("разные запросы получили одинаковый ключ идемпотентности")
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/gw/messages", strings.NewReader(`{"text":"y"}`))
	req.Header.Set("Idempotency-Key", "клиентский-ключ")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("запрос к gateway не удался: %v", err)
	}
	_ = resp.Body.Close()
	if got := gotKeys[len(gotKeys)-1]; got != "клиентский-ключ" {
		t.Fatalf("клиентский ключ идемпотентности потерян, получен %q", got)
	}
}

func TestSpecMatchesGatewayRoutes(t *testing.T) {
	g := &gateway{}
	registered := make(map[string]bool)
	for pattern := range g.publicRoutes() {
		registered[pattern] = true
	}

	spec := make(map[string]bool)
	for _, route := range apidocs.SpecRoutes() {
		if strings.Contains(route, " /gw/") {
			spec[route] = true
		}
	}

	for route := range registered {
		if !spec[route] {
			t.Errorf("маршрут %q зарегистрирован в gateway, но отсутствует в openapi.yaml", route)
		}
	}
	for route := range spec {
		if !registered[route] {
			t.Errorf("маршрут %q описан в openapi.yaml, но не зарегистрирован в gateway", route)
		}
	}
}
