package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/evgenza/otus-app/internal/observability"
)

var (
	appURL     string
	httpClient = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

func main() {
	observability.SetupLogger("otus-gateway")
	if err := run(); err != nil {
		slog.Error("фатальная ошибка", "err", err)
		os.Exit(1)
	}
}

func run() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}
	appURL = os.Getenv("APP_URL")
	if appURL == "" {
		appURL = "http://app:8080"
	}

	ctx := context.Background()
	shutdownTracing, err := observability.SetupTracing(ctx, "otus-gateway")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /gw/messages", proxyCreate)
	mux.HandleFunc("GET /gw/messages", proxyList)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           observability.WrapHTTP("otus-gateway", mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("gateway запущен", "port", port, "app_url", appURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-stop:
	}

	slog.Info("останавливаюсь...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "работает"})
}

func proxyCreate(w http.ResponseWriter, r *http.Request) {
	forward(w, r, http.MethodPost, r.Body)
}

func proxyList(w http.ResponseWriter, r *http.Request) {
	forward(w, r, http.MethodGet, nil)
}

func forward(w http.ResponseWriter, r *http.Request, method string, body io.Reader) {
	req, err := http.NewRequestWithContext(r.Context(), method, appURL+"/messages", body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось собрать запрос")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(r.Context(), "запрос к app не удался", "err", err)
		writeError(w, http.StatusBadGateway, "app недоступен")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
