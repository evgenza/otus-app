package broker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/evgenza/otus-app/internal/domain/messaging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Event - событие о созданном сообщении, общий формат для всех брокеров.
type Event = messaging.MessageCreated

// Publisher - отправка события в конкретный брокер.
type Publisher interface {
	Name() string
	Publish(ctx context.Context, ev Event) error
	Close() error
}

// Consumer - чтение событий из конкретного брокера.
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

	// Consumed - счетчик обработанных событий на стороне воркера.
	Consumed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_broker_consumed_total",
		Help: "Количество событий, вычитанных из брокера",
	}, []string{"broker", "result"})

	// Lag - время от создания события до его обработки воркером.
	Lag = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otus_broker_lag_seconds",
		Help:    "Задержка доставки события от публикации до обработки",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"broker"})
)

// ConfiguredNames не зависит от соединения: недоступный при запуске брокер
// все равно должен получать задания в outbox.
func ConfiguredNames() []string {
	var names []string
	for _, entry := range []struct{ name, key string }{{"kafka", "KAFKA_BROKERS"}, {"rabbitmq", "RABBITMQ_URLS"}, {"nats", "NATS_URLS"}} {
		if len(envList(entry.key)) > 0 {
			names = append(names, entry.name)
		}
	}
	return names
}

func NewPublisher(ctx context.Context, name string) (Publisher, error) {
	var p Publisher
	var err error
	switch name {
	case "kafka":
		p, err = newKafkaPublisher(ctx)
	case "rabbitmq":
		p, err = newRabbitPublisher(ctx)
	case "nats":
		p, err = newNATSPublisher(ctx)
	default:
		return nil, errUnknownBroker(name)
	}
	if err != nil || p == nil {
		return p, err
	}
	return &measuredPublisher{Publisher: p}, nil
}

type measuredPublisher struct{ Publisher }

func (p *measuredPublisher) Publish(ctx context.Context, ev Event) error {
	start := time.Now()
	err := p.Publisher.Publish(ctx, ev)
	publishDuration.WithLabelValues(p.Name()).Observe(time.Since(start).Seconds())
	result := "ok"
	if err != nil {
		result = "error"
	}
	published.WithLabelValues(p.Name(), result).Inc()
	return err
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
