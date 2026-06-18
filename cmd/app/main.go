package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/storage"
	"github.com/evgenza/otus-app/internal/version"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("не задана переменная DATABASE_URL")
	}

	ctx := context.Background()
	store, err := storage.New(ctx, dsn)
	if err != nil {
		log.Fatalf("не удалось подключиться к базе: %v", err)
	}
	defer store.Close()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handlers.New(store),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("otus-app %s слушает порт :%s", version.Version, port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ошибка сервера: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("останавливаюсь...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("не удалось корректно остановиться: %v", err)
		return
	}
	log.Println("сервер остановлен")
}
