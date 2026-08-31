// Воркер: читает события из брокера (Kafka, RabbitMQ или NATS) и
// раскладывает их в Cassandra и Elasticsearch. Масштабируется числом
// реплик контейнера и числом воркеров внутри одной реплики.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/evgenza/otus-app/internal/broker"
	"github.com/evgenza/otus-app/internal/cstore"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/search"
	"github.com/evgenza/otus-app/internal/version"
)

func main() {
	observability.SetupLogger("otus-consumer")
	if err := run(); err != nil {
		slog.Error("фатальная ошибка", "err", err)
		os.Exit(1)
	}
}

func run() error {
	brokerName := os.Getenv("BROKER")
	if brokerName == "" {
		brokerName = "kafka"
	}
	workers, _ := strconv.Atoi(os.Getenv("CONSUMER_WORKERS"))
	if workers <= 0 {
		workers = 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	events, err := cstore.New(ctx)
	if err != nil {
		return err
	}
	if events != nil {
		defer events.Close()
	}
	index, err := search.New(ctx)
	if err != nil {
		return err
	}
	if events == nil && index == nil {
		return errors.New("не настроено ни одно хранилище: нужны CASSANDRA_HOSTS или ELASTIC_URLS")
	}

	srv := metricsServer()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("сервер метрик остановился", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	handle := func(ctx context.Context, ev broker.Event) error {
		// Обработка идемпотентна: в Cassandra ключ — id события, в
		// Elasticsearch документ пишется под тем же id. Повторная
		// доставка после отказа узла не плодит дубликаты.
		if events != nil {
			if err := events.Save(ctx, ev, brokerName); err != nil {
				return err
			}
		}
		if index != nil {
			return index.IndexMessage(ctx, search.Document{
				ID:        ev.ID,
				Text:      ev.Text,
				Checksum:  ev.Checksum,
				Producer:  brokerName,
				CreatedAt: ev.CreatedAt,
			})
		}
		return nil
	}

	slog.Info("воркер запущен", "version", version.Version, "broker", brokerName, "workers", workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		consumer, err := broker.NewConsumer(ctx, brokerName)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func(n int, c broker.Consumer) {
			defer wg.Done()
			defer func() { _ = c.Close() }()
			if err := c.Consume(ctx, handle); err != nil && ctx.Err() == nil {
				slog.Error("воркер остановился с ошибкой", "worker", n, "err", err)
			}
		}(i, consumer)
	}

	<-ctx.Done()
	slog.Info("останавливаюсь...")
	wg.Wait()
	return nil
}

func metricsServer() *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"работает"}`))
	})
	port := os.Getenv("METRICS_PORT")
	if port == "" {
		port = "8091"
	}
	return &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}
