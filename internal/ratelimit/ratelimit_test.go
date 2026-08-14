package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type fakeCounter struct {
	count int64
	fail  bool
}

func (f *fakeCounter) Hit(context.Context, string, time.Duration) (int64, error) {
	if f.fail {
		return 0, errors.New("хранилище счетчиков недоступно")
	}
	f.count++
	return f.count, nil
}

func doRequest(l *Limiter) *httptest.ResponseRecorder {
	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestLimiterAllowsUnderLimit(t *testing.T) {
	l := NewWithCounter(&fakeCounter{}, 3, time.Minute)
	for i := 0; i < 3; i++ {
		if rec := doRequest(l); rec.Code != http.StatusOK {
			t.Fatalf("запрос %d: ожидался статус 200, получен %d", i+1, rec.Code)
		}
	}
}

func TestLimiterBlocksOverLimit(t *testing.T) {
	l := NewWithCounter(&fakeCounter{}, 2, time.Minute)
	doRequest(l)
	doRequest(l)
	if rec := doRequest(l); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("ожидался статус 429, получен %d", rec.Code)
	}
}

func TestLimiterFailOpen(t *testing.T) {
	l := NewWithCounter(&fakeCounter{fail: true}, 1, time.Minute)
	for i := 0; i < 3; i++ {
		if rec := doRequest(l); rec.Code != http.StatusOK {
			t.Fatalf("при недоступном хранилище запросы должны проходить, получен %d", rec.Code)
		}
	}
}

func TestLimiterSetLimit(t *testing.T) {
	l := NewWithCounter(&fakeCounter{}, 1, time.Minute)
	doRequest(l)
	if rec := doRequest(l); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("ожидался статус 429, получен %d", rec.Code)
	}
	l.SetLimit(10)
	if rec := doRequest(l); rec.Code != http.StatusOK {
		t.Fatalf("после повышения лимита ожидался статус 200, получен %d", rec.Code)
	}
	l.SetLimit(0)
	if got := l.Limit(); got != 10 {
		t.Fatalf("нулевой лимит не должен применяться, текущий лимит %d", got)
	}
}

func TestNilLimiterPassesThrough(t *testing.T) {
	var l *Limiter
	if rec := doRequest(l); rec.Code != http.StatusOK {
		t.Fatalf("nil-лимитер должен пропускать запросы, получен %d", rec.Code)
	}
}

func newValkeyCounter(t *testing.T) (*valkeyCounter, *miniredis.Miniredis) {
	t.Helper()
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &valkeyCounter{client: client}, srv
}

func TestValkeyCounterIncrements(t *testing.T) {
	counter, _ := newValkeyCounter(t)
	for want := int64(1); want <= 3; want++ {
		got, err := counter.Hit(context.Background(), "rl:тест", time.Minute)
		if err != nil {
			t.Fatalf("Hit вернул ошибку: %v", err)
		}
		if got != want {
			t.Fatalf("ожидался счетчик %d, получен %d", want, got)
		}
	}
}

func TestValkeyCounterSetsTTLOnce(t *testing.T) {
	counter, srv := newValkeyCounter(t)
	ctx := context.Background()

	if _, err := counter.Hit(ctx, "rl:тест", time.Minute); err != nil {
		t.Fatalf("Hit вернул ошибку: %v", err)
	}
	if ttl := srv.TTL("rl:тест"); ttl != time.Minute {
		t.Fatalf("первый запрос должен выставить TTL окна, получен %v", ttl)
	}

	srv.FastForward(30 * time.Second)
	if _, err := counter.Hit(ctx, "rl:тест", time.Minute); err != nil {
		t.Fatalf("Hit вернул ошибку: %v", err)
	}
	if ttl := srv.TTL("rl:тест"); ttl != 30*time.Second {
		t.Fatalf("повторные запросы не должны продлевать TTL, получен %v", ttl)
	}
}

func TestValkeyCounterWindowExpires(t *testing.T) {
	counter, srv := newValkeyCounter(t)
	ctx := context.Background()

	_, _ = counter.Hit(ctx, "rl:тест", time.Minute)
	_, _ = counter.Hit(ctx, "rl:тест", time.Minute)
	srv.FastForward(61 * time.Second)

	got, err := counter.Hit(ctx, "rl:тест", time.Minute)
	if err != nil {
		t.Fatalf("Hit вернул ошибку: %v", err)
	}
	if got != 1 {
		t.Fatalf("после истечения окна счетчик должен начаться заново, получен %d", got)
	}
	if ttl := srv.TTL("rl:тест"); ttl != time.Minute {
		t.Fatalf("новое окно должно получить свежий TTL, получен %v", ttl)
	}
}
