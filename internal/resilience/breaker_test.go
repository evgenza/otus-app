package resilience

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBreakerRecoveryAllowsOneProbe(t *testing.T) {
	name := t.Name()
	b := NewBreaker(name)
	now := time.Now()
	b.now = func() time.Time { return now }
	ctx := context.Background()
	failure := errors.New("сервис недоступен")
	for range 3 {
		_ = b.Do(ctx, func() error { return failure })
	}
	if got := testutil.ToFloat64(breakerTrips.WithLabelValues(name)); got != 1 {
		t.Fatalf("открытий: %v", got)
	}
	if err := b.Do(ctx, func() error { t.Fatal("открытая цепь пропустила вызов"); return nil }); !errors.Is(err, ErrOpen) {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- b.Do(ctx, func() error { close(entered); <-release; return nil }) }()
	<-entered
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := b.Do(ctx, func() error { t.Error("запущена лишняя проба"); return nil }); !errors.Is(err, ErrOpen) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(breakerState.WithLabelValues(name)); got != 0 {
		t.Fatalf("цепь не закрылась: %v", got)
	}
	for _, pair := range [][2]string{{"closed", "open"}, {"open", "half_open"}, {"half_open", "closed"}} {
		if got := testutil.ToFloat64(breakerTransitions.WithLabelValues(name, pair[0], pair[1])); got != 1 {
			t.Fatalf("переход %v: %v", pair, got)
		}
	}
	if got := testutil.ToFloat64(breakerCalls.WithLabelValues(name, "rejected")); got != 21 {
		t.Fatalf("отклонений: %v", got)
	}
}

func TestBreakerIgnoresOldSuccessAndReopensAfterFailedProbe(t *testing.T) {
	b := NewBreaker(t.Name())
	now := time.Now()
	b.now = func() time.Time { return now }
	ctx := context.Background()
	old, ok := b.admit()
	if !ok {
		t.Fatal("первый вызов отклонен")
	}
	failure := errors.New("сервис недоступен")
	for range 3 {
		_ = b.Do(ctx, func() error { return failure })
	}
	b.complete(old, nil)
	if b.state != open {
		t.Fatal("старый ответ закрыл цепь")
	}
	now = now.Add(11 * time.Second)
	_ = b.Do(ctx, func() error { return failure })
	if b.state != open {
		t.Fatal("неудачная проба не открыла цепь")
	}
	now = now.Add(11 * time.Second)
	_ = b.Do(ctx, func() error { return context.Canceled })
	if err := b.Do(ctx, func() error { return nil }); err != nil {
		t.Fatalf("отмена заблокировала пробу: %v", err)
	}
	if b.state != closed {
		t.Fatal("цепь не восстановилась")
	}
}
