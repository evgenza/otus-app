//go:build integration

package storage_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/storage"
)

func newStore(t *testing.T) *storage.Postgres {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL не задан — интеграционный тест пропущен")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := storage.New(ctx, dsn)
	if err != nil {
		t.Fatalf("не удалось подключиться к базе: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestStorageCreateAndList(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "интеграционное сообщение")
	if err != nil {
		t.Fatalf("Create вернул ошибку: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("ожидался ненулевой ID")
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List вернул ошибку: %v", err)
	}
	found := false
	for _, m := range list {
		if m.ID == created.ID && m.Text == "интеграционное сообщение" {
			found = true
			if !m.ChecksumOK {
				t.Error("контрольная сумма нетронутого сообщения должна сходиться")
			}
		}
	}
	if !found {
		t.Fatal("созданное сообщение не найдено в списке")
	}
}

func TestChecksumDetectsTampering(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "неизменное сообщение")
	if err != nil {
		t.Fatalf("Create вернул ошибку: %v", err)
	}

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("не удалось подключиться к базе: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET text = 'подменённый текст' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("не удалось подменить текст: %v", err)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List вернул ошибку: %v", err)
	}
	for _, m := range list {
		if m.ID == created.ID {
			if m.ChecksumOK {
				t.Error("подмена текста не обнаружена по контрольной сумме")
			}
			return
		}
	}
	t.Fatal("сообщение не найдено в списке")
}

func TestAPIWithDatabase(t *testing.T) {
	store := newStore(t)
	srv := httptest.NewServer(handlers.New(store, nil))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/messages", "application/json",
		strings.NewReader(`{"text":"e2e через api"}`))
	if err != nil {
		t.Fatalf("POST /messages: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ожидался статус 201, получен %d", resp.StatusCode)
	}

	listResp, err := http.Get(srv.URL + "/messages")
	if err != nil {
		t.Fatalf("GET /messages: %v", err)
	}
	defer func() { _ = listResp.Body.Close() }()

	var msgs []handlers.Message
	if err := json.NewDecoder(listResp.Body).Decode(&msgs); err != nil {
		t.Fatalf("декодирование списка: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("ожидалось хотя бы одно сообщение после POST")
	}
}
