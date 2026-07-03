package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/httpserver"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/storage"
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

	store, err := storage.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handlers.New(store),
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("сервис запущен", "version", version.Version, "port", port)
	return httpserver.Run(srv)
}
