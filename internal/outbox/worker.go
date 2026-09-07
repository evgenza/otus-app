// Package outbox доставляет зафиксированные события как минимум один раз.
package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/evgenza/otus-app/internal/domain/messaging"
	"github.com/evgenza/otus-app/internal/resilience"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var ErrLeaseLost = errors.New("аренда задания outbox потеряна")

type Delivery struct {
	MessageID int64
	Target    string
	Token     string
	Attempts  int
	Event     messaging.MessageCreated
}

type Stats struct {
	Pending       int64
	OldestSeconds float64
}

type Store interface {
	Claim(context.Context, string, time.Duration) (*Delivery, error)
	Complete(context.Context, Delivery) error
	Retry(context.Context, Delivery, time.Duration) error
	OutboxStats(context.Context, string) (Stats, error)
}

type Publisher interface {
	Publish(context.Context, messaging.MessageCreated) error
	Close() error
}

type Factory func(context.Context, string) (Publisher, error)

var (
	pending       = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "otus_outbox_pending", Help: "Недоставленные задания, включая арендованные и отложенные"}, []string{"target"})
	oldest        = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "otus_outbox_oldest_pending_seconds", Help: "Возраст самого старого недоставленного задания"}, []string{"target"})
	deliveries    = promauto.NewCounterVec(prometheus.CounterOpts{Name: "otus_outbox_deliveries_total", Help: "Попытки доставки событий из outbox"}, []string{"target", "result"})
	storageErrors = promauto.NewCounterVec(prometheus.CounterOpts{Name: "otus_outbox_storage_errors_total", Help: "Ошибки операций outbox с базой данных"}, []string{"target"})
)

// Start запускает отдельный воркер на каждый брокер. Сетевые вызовы выполняются
// вне транзакций БД. Возвращенная функция отменяет и дожидается всех воркеров.
func Start(parent context.Context, store Store, targets []string, factory Factory) func() {
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Go(func() { run(ctx, store, target, factory) })
	}
	return func() { cancel(); wg.Wait() }
}

func run(ctx context.Context, store Store, target string, factory Factory) {
	var publisher Publisher
	defer func() {
		if publisher != nil {
			_ = publisher.Close()
		}
	}()
	attempt := 0
	nextStats := time.Time{}
	for ctx.Err() == nil {
		if time.Now().After(nextStats) {
			statCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			s, err := store.OutboxStats(statCtx, target)
			cancel()
			if err == nil {
				pending.WithLabelValues(target).Set(float64(s.Pending))
				oldest.WithLabelValues(target).Set(s.OldestSeconds)
			} else {
				storageErrors.WithLabelValues(target).Inc()
			}
			nextStats = time.Now().Add(time.Second)
		}
		claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		d, err := store.Claim(claimCtx, target, 45*time.Second)
		cancel()
		if err != nil {
			storageErrors.WithLabelValues(target).Inc()
			slog.ErrorContext(ctx, "не удалось арендовать задание outbox", "target", target, "err", err)
			attempt++
			if !resilience.Wait(ctx, resilience.Backoff(attempt)) {
				return
			}
			continue
		}
		if d == nil {
			if !resilience.Wait(ctx, 250*time.Millisecond) {
				return
			}
			continue
		}
		pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pubCtx, span := otel.Tracer("otus/outbox").Start(pubCtx, "outbox.publish")
		span.SetAttributes(attribute.String("messaging.destination.name", target), attribute.Int64("message.id", d.MessageID))
		if publisher == nil {
			publisher, err = factory(pubCtx, target)
		}
		if err == nil && publisher == nil {
			err = errors.New("публикатор не настроен")
		}
		if err == nil {
			err = publisher.Publish(pubCtx, d.Event)
		}
		if err != nil {
			span.RecordError(err)
		}
		span.End()
		cancel()
		finishCtx, finishCancel := context.WithTimeout(ctx, 3*time.Second)
		if err != nil {
			deliveries.WithLabelValues(target, "error").Inc()
			slog.WarnContext(ctx, "публикация outbox не удалась, задание сохранено для повтора", "target", target, "message_id", d.MessageID, "attempt", d.Attempts, "err", err)
			if publisher != nil {
				_ = publisher.Close()
				publisher = nil
			}
			delay := resilience.Backoff(max(d.Attempts, attempt+1))
			err = store.Retry(finishCtx, *d, delay)
			finishCancel()
			if err != nil {
				storageErrors.WithLabelValues(target).Inc()
			}
			attempt++
			// Общая пауза на брокер не дает накопленной очереди усиливать его перегрузку.
			if !resilience.Wait(ctx, delay) {
				return
			}
			continue
		}
		err = store.Complete(finishCtx, *d)
		finishCancel()
		if err != nil {
			storageErrors.WithLabelValues(target).Inc()
			slog.WarnContext(ctx, "не удалось завершить задание outbox, доставка может повториться", "target", target, "message_id", d.MessageID, "err", err)
		} else {
			deliveries.WithLabelValues(target, "ok").Inc()
			slog.InfoContext(ctx, "событие outbox доставлено", "target", target, "message_id", d.MessageID)
		}
		attempt = 0
	}
}
