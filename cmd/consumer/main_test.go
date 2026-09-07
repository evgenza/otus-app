package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/evgenza/otus-app/internal/broker"
)

type testConsumer struct {
	fail   bool
	closed bool
	cancel context.CancelFunc
}

func (*testConsumer) Name() string { return "nats" }
func (c *testConsumer) Consume(ctx context.Context, handle func(context.Context, broker.Event) error) error {
	if c.fail {
		return errors.New("читатель остановился")
	}
	err := handle(ctx, broker.Event{ID: 42})
	c.cancel()
	return err
}
func (c *testConsumer) Close() error { c.closed = true; return nil }

func TestSupervisorRestartsAndClosesConsumers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failed := &testConsumer{fail: true}
	healthy := &testConsumer{cancel: cancel}
	attempt := 0
	handled := false
	supervise(ctx, 0, "nats", func(context.Context, string) (broker.Consumer, error) {
		attempt++
		if attempt == 1 {
			return failed, nil
		}
		return healthy, nil
	}, func(_ context.Context, ev broker.Event) error { handled = ev.ID == 42; return nil })
	if !handled || !failed.closed || !healthy.closed || attempt != 2 {
		t.Fatalf("читатель не восстановился: попытки=%d, обработано=%v", attempt, handled)
	}
}
