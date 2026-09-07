package resilience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
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

func TestJitterKeepsSpreadAtLimit(t *testing.T) {
	for _, attempt := range []int{1, 3, 1000000} {
		seen := map[time.Duration]bool{}
		capDelay := min(50*time.Millisecond*time.Duration(1<<min(attempt-1, 5)), time.Second)
		for range 100 {
			delay := Jitter(attempt, 50*time.Millisecond, time.Second)
			if delay < capDelay/2 || delay >= capDelay {
				t.Fatalf("попытка %d: задержка %v", attempt, delay)
			}
			seen[delay] = true
		}
		if len(seen) < 2 {
			t.Fatal("нет случайного разброса задержки")
		}
	}
}

func TestRetryBudgetAndMetrics(t *testing.T) {
	failure := errors.New("временный отказ")
	p := Policy{Attempts: 3, Base: time.Nanosecond, Max: time.Nanosecond}
	calls := 0
	err := p.Do(context.Background(), t.Name(), "write", func(error) bool { return true }, func() error { calls++; return failure })
	if !errors.Is(err, failure) || calls != 3 {
		t.Fatalf("попыток %d, ошибка %v", calls, err)
	}
	if got := testutil.ToFloat64(retries.WithLabelValues(t.Name(), "write")); got != 2 {
		t.Fatalf("повторов: %v", got)
	}
	if got := testutil.ToFloat64(exhausted.WithLabelValues(t.Name(), "write")); got != 1 {
		t.Fatalf("исчерпаний: %v", got)
	}
	calls = 0
	err = p.Do(context.Background(), t.Name(), "permanent", func(error) bool { return false }, func() error { calls++; return failure })
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatal("постоянная ошибка была повторена")
	}
}

func TestRetryCancellationDoesNotExhaustBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := Policy{Attempts: 3, Base: time.Hour, Max: time.Hour}
	err := p.Do(ctx, t.Name(), "call", func(error) bool { return true }, func() error { cancel(); return errors.New("отказ") })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(exhausted.WithLabelValues(t.Name(), "call")); got != 0 {
		t.Fatal("отмена учтена как исчерпание")
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
