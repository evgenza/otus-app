//go:build integration

package storage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evgenza/otus-app/internal/handlers"
)

func TestPostgresPartialFailureAndRecovery(t *testing.T) {
	store, pool, _ := outboxStore(t, "nats")
	ctx := context.Background()
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, "LOCK TABLE messages IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	api := handlers.New(store, nil, handlers.WithReadiness(store.Ping))
	var blocked atomic.Bool
	blocked.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timeout := 5 * time.Second
		if blocked.Load() {
			timeout = 200 * time.Millisecond
		}
		requestCtx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		api.ServeHTTP(w, r.WithContext(requestCtx))
	}))
	defer srv.Close()
	post := func() (int, int64, error) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/messages", strings.NewReader(`{"text":"одна запись после отказа"}`))
		req.Header.Set("Idempotency-Key", "partial-failure")
		resp, err := srv.Client().Do(req)
		if err != nil {
			return 0, 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		var message handlers.Message
		err = json.NewDecoder(resp.Body).Decode(&message)
		return resp.StatusCode, message.ID, err
	}
	code, _, err := post()
	if err != nil || code != http.StatusInternalServerError {
		t.Fatalf("блокировка записи: статус %d, ошибка %v", code, err)
	}
	resp, err := srv.Client().Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("блокировка таблицы ошибочно принята за полный отказ БД")
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	blocked.Store(false)
	for _, table := range []string{"messages", "message_outbox"} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("после таймаута в %s осталось %d записей: %v", table, count, err)
		}
	}
	var wg sync.WaitGroup
	ids := make(chan int64, 8)
	for range 8 {
		wg.Go(func() {
			code, id, err := post()
			if err != nil || code != http.StatusCreated {
				t.Errorf("восстановление: статус %d, ошибка %v", code, err)
				return
			}
			ids <- id
		})
	}
	wg.Wait()
	close(ids)
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		if id != first {
			t.Fatal("конкурентный повтор создал дубликат")
		}
	}
	var messages, events int
	err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM messages WHERE text='одна запись после отказа'),
		(SELECT count(*) FROM message_outbox WHERE message_id=$1 AND payload->>'text'='одна запись после отказа')`, first).Scan(&messages, &events)
	if err != nil || messages != 1 || events != 1 {
		t.Fatalf("итог: сообщений %d, событий %d, ошибка %v", messages, events, err)
	}
}

func TestLostHTTPResponseDoesNotDuplicateCommittedOperation(t *testing.T) {
	store, pool, _ := outboxStore(t, "nats")
	api := handlers.New(store, nil)
	var drop atomic.Bool
	drop.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if drop.Swap(false) {
			recorder := httptest.NewRecorder()
			api.ServeHTTP(recorder, r)
			if recorder.Code != http.StatusCreated {
				t.Errorf("операция не зафиксирована: %d", recorder.Code)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}
	post := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/messages", strings.NewReader(`{"text":"ответ потерян после commit"}`))
		req.Header.Set("Idempotency-Key", "lost-response")
		return client.Do(req)
	}
	if response, err := post(); err == nil {
		_ = response.Body.Close()
		t.Fatal("ответ должен потеряться после commit")
	}
	response, err := post()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var message handlers.Message
	if err := json.NewDecoder(response.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || message.ID == 0 {
		t.Fatalf("повтор: %d, %+v", response.StatusCode, message)
	}
	for _, table := range []string{"messages", "message_outbox"} {
		var count int
		if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil || count != 1 {
			t.Fatalf("в %s записей %d: %v", table, count, err)
		}
	}
}
