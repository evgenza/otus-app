package resilience

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPRetriesOnlyReplayableSafeRequests(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "данные" {
			t.Error("повтор потерял тело запроса")
		}
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		calls.Store(0)
		transport := NewHTTPTransport(t.Name()+method, nil)
		transport.policy.Base, transport.policy.Max = time.Nanosecond, time.Nanosecond
		req, _ := http.NewRequest(method, srv.URL, bytes.NewBufferString("данные"))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		want := int64(3)
		if method == http.MethodPost {
			want = 1
		}
		if calls.Load() != want {
			t.Fatalf("метод %s: попыток %d", method, calls.Load())
		}
	}
}

func TestHTTPBreakerRecovery(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	transport := NewHTTPTransport(t.Name(), nil)
	transport.policy.Base, transport.policy.Max = time.Nanosecond, time.Nanosecond
	now := time.Now()
	transport.breaker.now = func() time.Time { return now }
	call := func() error {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	for range 3 {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(); !errors.Is(err, ErrOpen) {
		t.Fatalf("ожидалось отклонение: %v", err)
	}
	if calls.Load() != 9 {
		t.Fatalf("число запросов: %d", calls.Load())
	}
	healthy.Store(true)
	now = now.Add(11 * time.Second)
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if transport.breaker.state != closed {
		t.Fatal("цепь не закрылась после восстановления")
	}
}
