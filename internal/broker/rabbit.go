package broker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Очередь объявляется quorum-типом: она реплицируется по узлам кластера
var quorumArgs = amqp.Table{"x-queue-type": "quorum"}

func rabbitSettings() (urls []string, queue string, prefetch int) {
	urls = envList("RABBITMQ_URLS")
	queue = env("RABBITMQ_QUEUE", "otus.messages")
	prefetch, _ = strconv.Atoi(env("RABBITMQ_PREFETCH", "32"))
	return
}

// rabbitConn — соединение с кластером, которое умеет переподключаться:
type rabbitConn struct {
	urls  []string
	queue string

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

func (r *rabbitConn) channel(confirm bool) (*amqp.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ch != nil && !r.ch.IsClosed() && r.conn != nil && !r.conn.IsClosed() {
		return r.ch, nil
	}
	r.closeLocked()

	var lastErr error
	for _, url := range r.urls {
		conn, err := amqp.DialConfig(url, amqp.Config{
			Heartbeat: 5 * time.Second,
			Dial:      amqp.DefaultDial(5 * time.Second),
		})
		if err != nil {
			lastErr = err
			continue
		}
		ch, err := conn.Channel()
		if err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		if _, err := ch.QueueDeclare(r.queue, true, false, false, false, quorumArgs); err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		if confirm {
			if err := ch.Confirm(false); err != nil {
				_ = conn.Close()
				lastErr = err
				continue
			}
		}
		r.conn, r.ch = conn, ch
		return ch, nil
	}
	if lastErr == nil {
		lastErr = errors.New("не задана переменная RABBITMQ_URLS")
	}
	return nil, lastErr
}

func (r *rabbitConn) closeLocked() {
	if r.ch != nil {
		_ = r.ch.Close()
		r.ch = nil
	}
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
}

func (r *rabbitConn) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
	return nil
}

type rabbitPublisher struct {
	*rabbitConn
}

func newRabbitPublisher(_ context.Context) (Publisher, error) {
	urls, queue, _ := rabbitSettings()
	if len(urls) == 0 {
		return nil, nil
	}
	p := &rabbitPublisher{&rabbitConn{urls: urls, queue: queue}}
	if _, err := p.channel(true); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *rabbitPublisher) Name() string { return "rabbitmq" }

func (r *rabbitPublisher) Publish(ctx context.Context, ev Event) error {
	payload, err := encode(ev)
	if err != nil {
		return err
	}
	// Две попытки: первая может упасть на соединении, которое умерло вместе
	// с узлом, вторая уже пойдет по свежему.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ch, err := r.channel(true)
		if err != nil {
			lastErr = err
			continue
		}
		conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", r.queue, false, false,
			amqp.Publishing{
				ContentType:  "application/json",
				DeliveryMode: amqp.Persistent,
				MessageId:    strconv.FormatInt(ev.ID, 10),
				Timestamp:    time.Now(),
				Body:         payload,
			})
		if err != nil {
			lastErr = err
			r.mu.Lock()
			r.closeLocked()
			r.mu.Unlock()
			continue
		}
		ok, err := conf.WaitContext(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("брокер не подтвердил публикацию")
		}
		return nil
	}
	return lastErr
}

type rabbitConsumer struct {
	*rabbitConn
	prefetch int
}

func newRabbitConsumer(_ context.Context) (Consumer, error) {
	urls, queue, prefetch := rabbitSettings()
	if len(urls) == 0 {
		return nil, errors.New("не задана переменная RABBITMQ_URLS")
	}
	return &rabbitConsumer{rabbitConn: &rabbitConn{urls: urls, queue: queue}, prefetch: prefetch}, nil
}

func (r *rabbitConsumer) Name() string { return "rabbitmq" }

func (r *rabbitConsumer) Consume(ctx context.Context, handle func(context.Context, Event) error) error {
	for ctx.Err() == nil {
		ch, err := r.channel(false)
		if err != nil {
			slog.WarnContext(ctx, "нет соединения с RabbitMQ", "err", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if err := ch.Qos(r.prefetch, 0, false); err != nil {
			r.reset()
			continue
		}
		deliveries, err := ch.ConsumeWithContext(ctx, r.queue, "", false, false, false, false, nil)
		if err != nil {
			r.reset()
			time.Sleep(time.Second)
			continue
		}
		for d := range deliveries {
			ev, err := decode(d.Body)
			if err != nil {
				Consumed.WithLabelValues("rabbitmq", "malformed").Inc()
				_ = d.Reject(false)
				continue
			}
			if err := handle(ctx, ev); err != nil {
				Consumed.WithLabelValues("rabbitmq", "error").Inc()
				slog.WarnContext(ctx, "не удалось обработать событие",
					"broker", "rabbitmq", "id", ev.ID, "err", err)
				_ = d.Nack(false, true) // вернуть в очередь
				continue
			}
			Consumed.WithLabelValues("rabbitmq", "ok").Inc()
			Lag.WithLabelValues("rabbitmq").Observe(time.Since(ev.CreatedAt).Seconds())
			_ = d.Ack(false)
		}
		// Канал закрылся — узел кластера ушел, идем на переподключение.
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "поток доставки RabbitMQ прерван, переподключаюсь")
			r.reset()
			time.Sleep(time.Second)
		}
	}
	return nil
}

func (r *rabbitConsumer) reset() {
	r.mu.Lock()
	r.closeLocked()
	r.mu.Unlock()
}
