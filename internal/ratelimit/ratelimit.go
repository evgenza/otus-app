package ratelimit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/evgenza/otus-app/internal/resilience"
	"github.com/redis/go-redis/v9"
)

type Counter interface {
	Hit(ctx context.Context, key string, window time.Duration) (int64, error)
}

type Limiter struct {
	counter Counter
	limit   atomic.Int64
	window  time.Duration
	breaker *resilience.Breaker
}

type valkeyCounter struct {
	client redis.UniversalClient
}

var hitScript = redis.NewScript(`local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return count`)

func (v *valkeyCounter) Hit(ctx context.Context, key string, window time.Duration) (int64, error) {
	return hitScript.Run(ctx, v.client, []string{key}, window.Milliseconds()).Int64()
}

func New() *Limiter {
	addrs := os.Getenv("VALKEY_ADDRS")
	if addrs == "" {
		return nil
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:      strings.Split(addrs, ","),
		MasterName: os.Getenv("VALKEY_MASTER_NAME"),
		// INCR мог выполниться до обрыва ответа. Его автоматический повтор даст двойной учет.
		MaxRetries:            -1,
		DialTimeout:           200 * time.Millisecond,
		ReadTimeout:           200 * time.Millisecond,
		WriteTimeout:          200 * time.Millisecond,
		ContextTimeoutEnabled: true,
	})
	limit := int64(100)
	if raw := os.Getenv("RATE_LIMIT"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			limit = n
		}
	}
	return NewWithCounter(&valkeyCounter{client: client}, limit, time.Minute)
}

func NewWithCounter(counter Counter, limit int64, window time.Duration) *Limiter {
	l := &Limiter{counter: counter, window: window, breaker: resilience.NewBreaker("redis")}
	l.limit.Store(limit)
	return l
}

func (l *Limiter) SetLimit(n int64) {
	if l == nil || n <= 0 {
		return
	}
	old := l.limit.Swap(n)
	if old != n {
		slog.Info("лимит запросов обновлён", "old", old, "new", n)
	}
}

func (l *Limiter) Limit() int64 {
	if l == nil {
		return 0
	}
	return l.limit.Load()
}

func (l *Limiter) Middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := "rl:" + clientIP(r) + ":" + strconv.FormatInt(time.Now().Unix()/int64(l.window.Seconds()), 10)
		ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
		defer cancel()

		var count int64
		err := l.breaker.Do(ctx, func() error {
			var err error
			count, err = l.counter.Hit(ctx, key, l.window)
			return err
		})
		if err != nil {
			slog.WarnContext(r.Context(), "рейт-лимитер недоступен, запрос пропущен", "err", err)
			next.ServeHTTP(w, r)
			return
		}
		if count > l.limit.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "слишком много запросов, попробуйте позже"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
