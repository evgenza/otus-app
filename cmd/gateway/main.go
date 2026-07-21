package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/evgenza/otus-app/internal/grpcapi"
	"github.com/evgenza/otus-app/internal/httpserver"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/security"
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
	appURL := os.Getenv("APP_URL")
	if appURL == "" {
		appURL = "http://app:8080"
	}
	appGRPCAddr := os.Getenv("APP_GRPC_ADDR")
	if appGRPCAddr == "" {
		appGRPCAddr = "app:9091"
	}

	ctx := context.Background()
	shutdownTracing, err := observability.SetupTracing(ctx, "otus-gateway")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	clientTLS, err := security.ClientTLS()
	if err != nil {
		return err
	}
	transport := http.DefaultTransport
	if clientTLS != nil {
		transport = &http.Transport{TLSClientConfig: clientTLS}
	}

	grpcClient, err := newGRPCClient(appGRPCAddr)
	if err != nil {
		return err
	}

	g := &gateway{
		appURL: appURL,
		client: &http.Client{
			Transport: otelhttp.NewTransport(transport),
			Timeout:   10 * time.Second,
		},
		grpc: grpcClient,
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           observability.WrapHTTP("otus-gateway", g.routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("gateway запущен", "port", port, "app_url", appURL, "app_grpc", appGRPCAddr)
	return httpserver.Run(srv)
}

type gateway struct {
	appURL string
	client *http.Client
	grpc   grpcapi.MessageServiceClient
}

func (g *gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /gw/messages", g.proxyCreate)
	mux.HandleFunc("GET /gw/messages", g.proxyList)
	mux.HandleFunc("POST /gw/grpc/messages", g.grpcCreate)
	mux.HandleFunc("GET /gw/grpc/messages", g.grpcList)
	mux.HandleFunc("POST /gw/grpc/messages/batch", g.grpcBatch)
	mux.HandleFunc("POST /gw/grpc/chat", g.grpcChat)
	return mux
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "работает"})
}

func (g *gateway) proxyCreate(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, http.MethodPost, r.Body)
}

func (g *gateway) proxyList(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, http.MethodGet, nil)
}

func (g *gateway) forward(w http.ResponseWriter, r *http.Request, method string, body io.Reader) {
	req, err := http.NewRequestWithContext(r.Context(), method, g.appURL+"/messages", body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось собрать запрос")
		return
	}
	copyHeaders(req.Header, r.Header)
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			host = prior + ", " + host
		}
		req.Header.Set("X-Forwarded-For", host)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		slog.ErrorContext(r.Context(), "запрос к app не удался", "err", err)
		writeError(w, http.StatusBadGateway, "app недоступен")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		if isHopHeader(name) {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

func isHopHeader(name string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
