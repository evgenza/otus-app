package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/evgenza/otus-app/internal/audit"
	"github.com/evgenza/otus-app/internal/blobstore"
	"github.com/evgenza/otus-app/internal/broker"
	"github.com/evgenza/otus-app/internal/coord"
	"github.com/evgenza/otus-app/internal/cstore"
	"github.com/evgenza/otus-app/internal/grpcserver"
	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/hdfsstore"
	"github.com/evgenza/otus-app/internal/httpserver"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/ratelimit"
	"github.com/evgenza/otus-app/internal/search"
	"github.com/evgenza/otus-app/internal/security"
	"github.com/evgenza/otus-app/internal/storage"
	"github.com/evgenza/otus-app/internal/tcache"
	"github.com/evgenza/otus-app/internal/version"
)

func main() {
	observability.SetupLogger("otus-app")
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

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("не задана переменная DATABASE_URL")
	}

	ctx := context.Background()
	shutdownTracing, err := observability.SetupTracing(ctx, "otus-app")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	coordinator, err := coord.New()
	if err != nil {
		return err
	}
	defer coordinator.Close()

	var store *storage.Postgres
	err = coordinator.WithLock(ctx, "/otus/lock/migrate", func() error {
		var lockErr error
		store, lockErr = storage.New(ctx, dsn)
		return lockErr
	})
	if err != nil {
		return err
	}
	defer store.Close()

	limiter := ratelimit.New()
	coordinator.WatchInt(ctx, "/otus/config/rate_limit", limiter.SetLimit)

	auditLog, err := audit.New(ctx)
	if err != nil {
		slog.Warn("аудит-лог недоступен, работаю без него", "err", err)
	}
	cache, err := tcache.New(ctx)
	if err != nil {
		slog.Warn("кэш недоступен, работаю без него", "err", err)
	}

	apiOpts := []handlers.Option{handlers.WithLimiter(limiter)}
	if auditLog != nil {
		apiOpts = append(apiOpts, handlers.WithAudit(auditLog))
	}
	if cache != nil {
		apiOpts = append(apiOpts, handlers.WithCache(cache))
	}

	// Брокеры и распределенные хранилища: каждое включается своими настройками.
	bus := broker.NewBus(ctx)
	defer bus.Close()
	if len(bus.Names()) > 0 {
		apiOpts = append(apiOpts, handlers.WithBus(bus))
	}

	blobs, err := blobstore.New(ctx)
	if err != nil {
		slog.Warn("S3 недоступен, работаю без него", "err", err)
	}
	if blobs != nil {
		apiOpts = append(apiOpts, handlers.WithBlobs(blobs))
	}

	files, err := hdfsstore.New(ctx)
	if err != nil {
		slog.Warn("HDFS недоступен, работаю без него", "err", err)
	}
	if files != nil {
		apiOpts = append(apiOpts, handlers.WithFiles(files))
	}

	events, err := cstore.New(ctx)
	if err != nil {
		slog.Warn("Cassandra недоступна, работаю без нее", "err", err)
	}
	if events != nil {
		defer events.Close()
		apiOpts = append(apiOpts, handlers.WithEvents(events))
	}

	index, err := search.New(ctx)
	if err != nil {
		slog.Warn("Elasticsearch недоступен, работаю без него", "err", err)
	}
	if index != nil {
		apiOpts = append(apiOpts, handlers.WithSearch(index))
	}

	tlsCfg, err := security.ServerTLS()
	if err != nil {
		return err
	}
	auth := security.NewAuth()

	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = "9091"
	}
	creds := insecure.NewCredentials()
	if tlsCfg != nil {
		creds = credentials.NewTLS(tlsCfg.Clone())
	}
	gsrv := grpcserver.New(store, auth, creds)
	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		return err
	}
	go func() {
		if err := gsrv.Serve(lis); err != nil {
			slog.Error("gRPC-сервер остановился с ошибкой", "err", err)
		}
	}()
	defer gsrv.GracefulStop()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handlers.New(store, auth, apiOpts...),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
	}

	slog.Info("сервис запущен",
		"version", version.Version, "port", port, "grpc_port", grpcPort, "mtls", tlsCfg != nil,
		"brokers", bus.Names())
	return httpserver.Run(srv)
}
