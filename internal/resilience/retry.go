package resilience

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff добавляет случайную задержку и ограничивает ее 30 секундами.
func Backoff(attempt int) time.Duration {
	capDelay := time.Second << min(max(attempt-1, 0), 5)
	capDelay = min(capDelay, 30*time.Second)
	return capDelay/2 + time.Duration(rand.Int64N(int64(capDelay/2)))
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
