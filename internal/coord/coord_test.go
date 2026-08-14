package coord

import (
	"context"
	"errors"
	"testing"
)

func TestNilCoordinatorRunsWithoutLock(t *testing.T) {
	var c *Coordinator
	called := false
	err := c.WithLock(context.Background(), "/otus/lock/test", func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock вернул ошибку: %v", err)
	}
	if !called {
		t.Fatal("функция под блокировкой не была вызвана")
	}
}

func TestNilCoordinatorPropagatesError(t *testing.T) {
	var c *Coordinator
	want := errors.New("ошибка изнутри")
	if err := c.WithLock(context.Background(), "/otus/lock/test", func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("ожидалась ошибка из функции, получено: %v", err)
	}
}

func TestNilCoordinatorWatchNoop(t *testing.T) {
	var c *Coordinator
	c.WatchInt(context.Background(), "/otus/config/rate_limit", func(int64) {
		t.Fatal("watch на nil-координаторе не должен ничего применять")
	})
}
