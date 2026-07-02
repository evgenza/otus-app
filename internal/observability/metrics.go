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
)

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
