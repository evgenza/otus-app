package resilience

import (
	"context"
	"testing"
	"time"
)

func TestBackoffBoundedForLongOutages(t *testing.T) {
	for _, attempt := range []int{-1, 0, 1, 5, 1000} {
		for range 100 {
			d := Backoff(attempt)
			if d < 500*time.Millisecond || d > 30*time.Second {
				t.Fatalf("задержка вне границ: %v", d)
			}
		}
	}
}

func TestCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if Wait(ctx, time.Hour) {
		t.Fatal("ожидание не заметило отмену")
	}
	if time.Since(start) > time.Second {
		t.Fatal("отмена ожидания заняла больше секунды")
	}
}
