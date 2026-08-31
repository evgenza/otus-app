package broker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Event — событие о созданном сообщении, общий формат для всех брокеров.
type Event struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"created_at"`
	Producer  string    `json:"producer,omitempty"`
}

// Publisher — отправка события в конкретный брокер.
type Publisher interface {
	Name() string
	Publish(ctx context.Context, ev Event) error
	Close() error
}

// Consumer — чтение событий из конкретного брокера.
// Consume блокируется до отмены контекста и вызывает handle на каждое
// событие; ошибка handle означает, что сообщение не подтверждается.
type Consumer interface {
	Name() string
	Consume(ctx context.Context, handle func(context.Context, Event) error) error
	Close() error
}

var (
	published = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_broker_published_total",
		Help: "Количество событий, отправленных в брокер",
	}, []string{"broker", "result"})

	publishDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otus_broker_publish_duration_seconds",
		Help:    "Время публикации события в брокер",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"broker"})

	// Consumed — счетчик обработанных событий на стороне воркера.
	Consumed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_broker_consumed_total",
		Help: "Количество событий, вычитанных из брокера",
	}, []string{"broker", "result"})

	// Lag — время от создания события до его обработки воркером.
	Lag = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otus_broker_lag_seconds",
		Help:    "Задержка доставки события от публикации до обработки",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"broker"})
)

// Bus рассылает событие сразу во все настроенные брокеры.
type Bus struct {
	publishers []Publisher
}

// NewBus поднимает публикаторы для тех брокеров, чьи адреса заданы в
// окружении. Недоступный брокер не мешает старту: он просто выпадает из
// шины с предупреждением в логе.
func NewBus(ctx context.Context) *Bus {
	bus := &Bus{}
	for _, factory := range []struct {
		name string
		make func(context.Context) (Publisher, error)
	}{
		{"kafka", newKafkaPublisher},
		{"rabbitmq", newRabbitPublisher},
		{"nats", newNATSPublisher},
	} {
		p, err := factory.make(ctx)
		if err != nil {
			slog.Warn("брокер недоступен, работаю без него", "broker", factory.name, "err", err)
			continue
		}
		if p == nil {
			continue
		}
		slog.Info("брокер подключен", "broker", p.Name())
		bus.publishers = append(bus.publishers, p)
	}
	return bus
}

// Names возвращает список подключенных брокеров.
func (b *Bus) Names() []string {
	if b == nil {
		return nil
	}
	names := make([]string, 0, len(b.publishers))
	for _, p := range b.publishers {
		names = append(names, p.Name())
	}
	return names
}

// Publish рассылает событие во все брокеры параллельно. Публикация
// best-effort: ошибка одного брокера не отменяет остальных и не валит
// запрос, но видна в метрике otus_broker_published_total.
func (b *Bus) Publish(ctx context.Context, ev Event) {
	if b == nil || len(b.publishers) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, p := range b.publishers {
		wg.Add(1)
		go func(p Publisher) {
			defer wg.Done()
			start := time.Now()
			err := p.Publish(ctx, ev)
			publishDuration.WithLabelValues(p.Name()).Observe(time.Since(start).Seconds())
			if err != nil {
				published.WithLabelValues(p.Name(), "error").Inc()
				slog.WarnContext(ctx, "не удалось опубликовать событие",
					"broker", p.Name(), "id", ev.ID, "err", err)
				return
			}
			published.WithLabelValues(p.Name(), "ok").Inc()
		}(p)
	}
	wg.Wait()
}

// Close закрывает соединения со всеми брокерами.
func (b *Bus) Close() {
	if b == nil {
		return
	}
	for _, p := range b.publishers {
		if err := p.Close(); err != nil {
			slog.Warn("ошибка при закрытии брокера", "broker", p.Name(), "err", err)
		}
	}
}

// NewConsumer создает читателя для брокера с указанным именем.
func NewConsumer(ctx context.Context, name string) (Consumer, error) {
	switch name {
	case "kafka":
		return newKafkaConsumer(ctx)
	case "rabbitmq", "rabbit":
		return newRabbitConsumer(ctx)
	case "nats":
		return newNATSConsumer(ctx)
	}
	return nil, errUnknownBroker(name)
}

func encode(ev Event) ([]byte, error) {
	return json.Marshal(ev)
}

func decode(raw []byte) (Event, error) {
	var ev Event
	err := json.Unmarshal(raw, &ev)
	return ev, err
}

func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

type errUnknownBroker string

func (e errUnknownBroker) Error() string { return "неизвестный брокер: " + string(e) }
