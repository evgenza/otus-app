package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/evgenza/otus-app/internal/httpserver"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/security"
)

var (
	trafficBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_proxy_traffic_bytes_total",
		Help: "Сколько байт полезной нагрузки прокси пропустил через себя",
	}, []string{"direction"})

	proxyRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_proxy_requests_total",
		Help: "Сколько запросов прошло через прокси",
	}, []string{"result"})

	faultsInjected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otus_proxy_faults_total",
		Help: "Сколько помех внёс прокси",
	}, []string{"type"})
)

type faults struct {
	DelayMs     int     `json:"delay_ms"`
	Repeat      bool    `json:"repeat"`
	DropRate    float64 `json:"drop_rate"`
	ThrottleBPS int     `json:"throttle_bps"`
}

type controls struct {
	mu sync.RWMutex
	f  faults
}

func (c *controls) get() faults {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.f
}

func (c *controls) set(f faults) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.f = f
}

func main() {
	observability.SetupLogger("otus-chaosproxy")
	if err := run(); err != nil {
		slog.Error("фатальная ошибка", "err", err)
		os.Exit(1)
	}
}

func run() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	controlPort := os.Getenv("CONTROL_PORT")
	if controlPort == "" {
		controlPort = "8091"
	}
	appURL := os.Getenv("APP_URL")
	if appURL == "" {
		appURL = "http://app:8080"
	}

	tlsCfg, err := security.ServerTLS()
	if err != nil {
		return err
	}
	clientTLS, err := security.ClientTLS()
	if err != nil {
		return err
	}
	transport := http.DefaultTransport
	if clientTLS != nil {
		transport = &http.Transport{TLSClientConfig: clientTLS}
	}

	p := &proxy{
		appURL: strings.TrimRight(appURL, "/"),
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second},
		ctl:    &controls{},
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /health", p.controlHealth)
		mux.HandleFunc("GET /control", p.controlGet)
		mux.HandleFunc("POST /control", p.controlSet)
		mux.Handle("GET /metrics", promhttp.Handler())
		srv := &http.Server{Addr: ":" + controlPort, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("управляющий сервер остановился", "err", err)
		}
	}()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           p,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
	}
	slog.Info("chaos-прокси запущен",
		"port", port, "control_port", controlPort, "app_url", appURL, "mtls", tlsCfg != nil)
	return httpserver.Run(srv)
}

type proxy struct {
	appURL string
	client *http.Client
	ctl    *controls
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f := p.ctl.get()

	if f.DropRate > 0 && rand.Float64() < f.DropRate {
		faultsInjected.WithLabelValues("drop").Inc()
		proxyRequests.WithLabelValues("dropped").Inc()
		slog.Warn("запрос оборван по команде drop", "path", r.URL.Path)
		writeError(w, http.StatusBadGateway, "запрос оборван прокси")
		return
	}
	if f.DelayMs > 0 {
		faultsInjected.WithLabelValues("delay").Inc()
		time.Sleep(time.Duration(f.DelayMs) * time.Millisecond)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "не удалось прочитать тело запроса")
		return
	}
	trafficBytes.WithLabelValues("to_app").Add(float64(len(body)))

	attempts := 1
	if f.Repeat && r.Method != http.MethodGet {
		attempts = 2
		faultsInjected.WithLabelValues("repeat").Inc()
		slog.Warn("запрос будет повторён по команде repeat", "path", r.URL.Path)
	}

	var resp *http.Response
	for i := 0; i < attempts; i++ {
		req, err := http.NewRequestWithContext(r.Context(), r.Method,
			p.appURL+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось собрать запрос")
			return
		}
		req.Header = r.Header.Clone()
		resp, err = p.client.Do(req)
		if err != nil {
			slog.Error("запрос к app не удался", "err", err)
			proxyRequests.WithLabelValues("error").Inc()
			writeError(w, http.StatusBadGateway, "app недоступен")
			return
		}
		if i < attempts-1 {
			n, _ := io.Copy(io.Discard, resp.Body)
			trafficBytes.WithLabelValues("from_app").Add(float64(n))
			_ = resp.Body.Close()
		}
	}
	defer func() { _ = resp.Body.Close() }()

	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	var written int64
	if f.ThrottleBPS > 0 {
		faultsInjected.WithLabelValues("throttle").Inc()
		written = copyThrottled(w, resp.Body, f.ThrottleBPS)
	} else {
		written, _ = io.Copy(w, resp.Body)
	}
	trafficBytes.WithLabelValues("from_app").Add(float64(written))
	proxyRequests.WithLabelValues("ok").Inc()
}

func copyThrottled(dst io.Writer, src io.Reader, bps int) int64 {
	chunk := bps / 10
	if chunk < 1 {
		chunk = 1
	}
	buf := make([]byte, chunk)
	var total int64
	flusher, _ := dst.(http.Flusher)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			written, werr := dst.Write(buf[:n])
			total += int64(written)
			if flusher != nil {
				flusher.Flush()
			}
			if werr != nil {
				return total
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			return total
		}
	}
}

func (p *proxy) controlHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "работает"})
}

func (p *proxy) controlGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, p.ctl.get())
}

func (p *proxy) controlSet(w http.ResponseWriter, r *http.Request) {
	var f faults
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, "некорректное тело запроса")
		return
	}
	if f.DelayMs < 0 || f.ThrottleBPS < 0 || f.DropRate < 0 || f.DropRate > 1 {
		writeError(w, http.StatusBadRequest, "недопустимые значения помех")
		return
	}
	p.ctl.set(f)
	slog.Info("помехи обновлены",
		"delay_ms", f.DelayMs, "repeat", f.Repeat, "drop_rate", f.DropRate, "throttle_bps", f.ThrottleBPS)
	writeJSON(w, http.StatusOK, f)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
