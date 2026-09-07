//go:build integration

package storage_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/evgenza/otus-app/internal/domain/messaging"
	"github.com/evgenza/otus-app/internal/outbox"
	"github.com/evgenza/otus-app/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Каждый тест работает в собственной схеме; существующие данные не затрагиваются.
func outboxStore(t *testing.T, targets ...string) (*storage.Postgres, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL не задан")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("test_outbox_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	dsn = u.String()
	p, err := storage.New(ctx, dsn, targets...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return p, pool, dsn
}

func TestOutboxAtomicRollback(t *testing.T) {
	s, pool, _ := outboxStore(t, "kafka", "некорректный-брокер")
	if _, err := s.Create(context.Background(), "атомарная запись", ""); err == nil {
		t.Fatal("ожидалась ошибка ограничения outbox")
	}
	for _, table := range []string{"messages", "message_outbox"} {
		var count int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("транзакция оставила %d строк в %s", count, table)
		}
	}
}

func TestOutboxConcurrentIdempotency(t *testing.T) {
	s, pool, _ := outboxStore(t, "kafka", "nats")
	var wg sync.WaitGroup
	ids := make(chan int64, 12)
	for range 12 {
		wg.Go(func() {
			m, err := s.Create(context.Background(), "одна команда", "общий-ключ")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- m.ID
		})
	}
	wg.Wait()
	close(ids)
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		if first != id {
			t.Fatal("повтор создал другое сообщение")
		}
	}
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM message_outbox").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("ожидалось по одному заданию на брокер, получено %d", count)
	}
	if _, err := s.Create(context.Background(), "другой текст", "общий-ключ"); !errors.Is(err, messaging.ErrIdempotencyConflict) {
		t.Fatalf("ожидался конфликт: %v", err)
	}
}

func TestOutboxLeaseRecoveryAndFencing(t *testing.T) {
	s, pool, dsn := outboxStore(t, "nats", "kafka")
	ctx := context.Background()
	if _, err := s.Create(ctx, "перезапуск воркера", ""); err != nil {
		t.Fatal(err)
	}
	old, err := s.Claim(ctx, "nats", 45*time.Second)
	if err != nil || old == nil {
		t.Fatalf("аренда: %v", err)
	}
	if d, err := s.Claim(ctx, "nats", time.Second); err != nil || d != nil {
		t.Fatalf("занятая строка выдана второй раз: %v", err)
	}
	// Другой брокер не блокируется чужой арендой.
	other, err := s.Claim(ctx, "kafka", time.Second)
	if err != nil || other == nil {
		t.Fatalf("изоляция брокеров: %v", err)
	}
	if err := s.Complete(ctx, *other); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE message_outbox SET available_at=now()-interval '1 second' WHERE target='nats'`); err != nil {
		t.Fatal(err)
	}
	restarted, err := storage.New(ctx, dsn, "nats", "kafka")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	fresh, err := restarted.Claim(ctx, "nats", time.Second)
	if err != nil || fresh == nil {
		t.Fatalf("восстановление аренды: %v", err)
	}
	if fresh.Token == old.Token || fresh.Attempts != 2 {
		t.Fatal("аренда не получила новый токен и номер попытки")
	}
	if err := s.Complete(ctx, *old); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("устаревший воркер завершил задание: %v", err)
	}
	if err := s.Retry(ctx, *old, time.Second); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("устаревший воркер изменил задание: %v", err)
	}
	if err := restarted.Complete(ctx, *fresh); err != nil {
		t.Fatal(err)
	}
	stats, err := s.OutboxStats(ctx, "nats")
	if err != nil || stats.Pending != 0 {
		t.Fatalf("очередь не опустела: %+v, %v", stats, err)
	}
}

func TestOutboxParallelClaims(t *testing.T) {
	s, _, _ := outboxStore(t, "nats")
	ctx := context.Background()
	for range 24 {
		if _, err := s.Create(ctx, "параллельная доставка", ""); err != nil {
			t.Fatal(err)
		}
	}
	ids := make(chan int64, 24)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				d, err := s.Claim(ctx, "nats", time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if d == nil {
					return
				}
				ids <- d.MessageID
				if err := s.Complete(ctx, *d); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("двойная аренда %d", id)
		}
		seen[id] = true
	}
	if len(seen) != 24 {
		t.Fatalf("доставлено %d из 24", len(seen))
	}
}

type recoveryPublisher struct {
	failures  int
	delivered chan int64
}

func (p *recoveryPublisher) Publish(_ context.Context, ev messaging.MessageCreated) error {
	if p.failures > 0 {
		p.failures--
		return errors.New("брокер временно недоступен")
	}
	p.delivered <- ev.ID
	return nil
}
func (*recoveryPublisher) Close() error { return nil }

func TestOutboxWorkerRecoversUnavailableAtStartup(t *testing.T) {
	s, _, _ := outboxStore(t, "nats")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m, err := s.Create(ctx, "доставка после восстановления", "")
	if err != nil {
		t.Fatal(err)
	}
	p := &recoveryPublisher{failures: 1, delivered: make(chan int64, 1)}
	starts := 0
	stop := outbox.Start(ctx, s, []string{"nats"}, func(context.Context, string) (outbox.Publisher, error) {
		starts++
		if starts == 1 {
			return nil, errors.New("брокер недоступен при запуске")
		}
		return p, nil
	})
	defer stop()
	select {
	case id := <-p.delivered:
		if id != m.ID {
			t.Fatal("доставлено другое событие")
		}
	case <-ctx.Done():
		t.Fatal("событие не восстановилось")
	}
	for {
		stats, err := s.OutboxStats(ctx, "nats")
		if err != nil {
			t.Fatal(err)
		}
		if stats.Pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("задание не завершено")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
