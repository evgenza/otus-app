package observability

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_http_requests_total",
		Help: "Количество обработанных HTTP-запросов",
	}, []string{"method", "route", "status"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otus_http_request_duration_seconds",
		Help:    "Время обработки HTTP-запросов",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	// MessagesCreated — бизнес-метрика: сколько сообщений сохранено.
	MessagesCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "otus_messages_created_total",
		Help: "Количество созданных сообщений",
	})

	storageOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_storage_ops_total",
		Help: "Количество обращений к распределенным хранилищам",
	}, []string{"backend", "op", "result"})

	storageDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otus_storage_op_duration_seconds",
		Help:    "Время обращения к распределенным хранилищам",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"backend", "op"})
)

// ObserveStorage пишет метрики одной операции с хранилищем
func ObserveStorage(backend, op string, start time.Time, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	storageOps.WithLabelValues(backend, op, result).Inc()
	storageDuration.WithLabelValues(backend, op).Observe(time.Since(start).Seconds())
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// WrapHTTP оборачивает роутер: трейс-спан (OTel), метрики Prometheus и
// структурный лог на каждый запрос.
func WrapHTTP(service string, next http.Handler) http.Handler {
	instrumented := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "other"
		}
		dur := time.Since(start)
		httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		httpDuration.WithLabelValues(r.Method, route).Observe(dur.Seconds())
		slog.InfoContext(r.Context(), "http_request",
			"method", r.Method,
			"route", route,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
		)
	})

	return otelhttp.NewHandler(instrumented, "http.server",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}
