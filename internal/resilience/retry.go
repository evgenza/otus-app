package resilience

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff добавляет случайную задержку и ограничивает ее 30 секундами.
func Backoff(attempt int) time.Duration {
	return Jitter(attempt, time.Second, 30*time.Second)
}

// Jitter сохраняет разброс от половины до полной задержки даже на верхнем пределе.
func Jitter(attempt int, base, limit time.Duration) time.Duration {
	base = max(base, time.Nanosecond)
	limit = max(limit, base)
	delay := base
	for n := 1; n < attempt && delay < limit; n++ {
		if delay > limit/2 {
			delay = limit
			break
		}
		delay *= 2
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)))
}

type Policy struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
}

// Do повторяет только операции, для которых вызывающий код подтвердил безопасность повтора.
func (p Policy) Do(ctx context.Context, dependency, operation string, retryable func(error) bool, call func() error) error {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 1 {
			Retried(dependency, operation)
		}
		err := call()
		if err == nil || !retryable(err) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= max(p.Attempts, 1) {
			Exhausted(dependency, operation)
			return err
		}
		if !Wait(ctx, Jitter(attempt, p.Base, p.Max)) {
			return ctx.Err()
		}
	}
}

func Wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
