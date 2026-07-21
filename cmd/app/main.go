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

	"github.com/evgenza/otus-app/internal/grpcserver"
	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/httpserver"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/security"
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
		Handler:           handlers.New(store, auth),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
	}

	slog.Info("сервис запущен",
		"version", version.Version, "port", port, "grpc_port", grpcPort, "mtls", tlsCfg != nil)
	return httpserver.Run(srv)
}
