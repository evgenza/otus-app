package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
