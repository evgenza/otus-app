//go:build integration

package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndexRecoveryAfterAppliedWriteAndLostReply(t *testing.T) {
	var unavailable atomic.Int64
	unavailable.Store(3)
	var drop atomic.Bool
	var mu sync.Mutex
	documents := map[string]Document{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if r.Method == http.MethodHead {
			return
		}
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/_doc/") {
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var doc Document
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		documents[r.URL.Path] = doc
		mu.Unlock()
		// Хранилище уже применило запись, но клиент не получил подтверждение.
		if unavailable.Add(-1) >= 0 {
			w.WriteHeader(503)
			return
		}
		if drop.Swap(false) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"result":"updated"}`))
	}))
	defer srv.Close()
	t.Setenv("ELASTIC_URLS", srv.URL)
	t.Setenv("ELASTIC_INDEX", "test-recovery")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	index, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := Document{ID: 71, Text: "сохранено до отказа", Checksum: "hash-71"}
	if err := index.IndexMessage(ctx, first); err == nil {
		t.Fatal("ожидалось исчерпание трех попыток")
	}
	if err := index.IndexMessage(ctx, first); err != nil {
		t.Fatalf("восстановление: %v", err)
	}
	drop.Store(true)
	second := Document{ID: 72, Text: "потеря ответа", Checksum: "hash-72"}
	if err := index.IndexMessage(ctx, second); err != nil {
		t.Fatalf("повтор после обрыва: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(documents) != 2 || documents["/test-recovery/_doc/71"] != first || documents["/test-recovery/_doc/72"] != second {
		t.Fatalf("итоговое состояние содержит потери или дубли: %+v", documents)
	}
}

func TestIndexRetriesAnotherClusterNode(t *testing.T) {
	var mu sync.Mutex
	var nodes []string
	var saved Document
	handler := func(node string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			if r.Method == http.MethodHead {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			nodes = append(nodes, node)
			if node == nodes[0] {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"result":"created"}`))
		}
	}
	first := httptest.NewServer(handler("первый"))
	defer first.Close()
	second := httptest.NewServer(handler("второй"))
	defer second.Close()
	t.Setenv("ELASTIC_URLS", first.URL+","+second.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	index, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{ID: 81, Text: "доступен один узел", Checksum: "hash-81"}
	if err := index.IndexMessage(ctx, doc); err != nil {
		t.Fatalf("не удалось переключиться на доступный узел: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(nodes) != 2 || nodes[0] == nodes[1] || saved != doc {
		t.Fatalf("узлы %v, сохраненный документ %+v", nodes, saved)
	}
}
