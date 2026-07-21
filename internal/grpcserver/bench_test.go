package grpcserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/evgenza/otus-app/internal/grpcapi"
	"github.com/evgenza/otus-app/internal/grpcserver"
	"github.com/evgenza/otus-app/internal/handlers"
)

func quietLogs(b *testing.B) {
	b.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(old) })
}

func newBenchGRPCClient(b *testing.B, store handlers.MessageStore) grpcapi.MessageServiceClient {
	b.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("не удалось открыть порт: %v", err)
	}
	srv := grpcserver.New(store, nil, nil)
	go func() { _ = srv.Serve(lis) }()
	b.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatalf("не удалось подключиться: %v", err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	return grpcapi.NewMessageServiceClient(conn)
}

func BenchmarkHTTPCreateMessage(b *testing.B) {
	quietLogs(b)
	srv := httptest.NewServer(handlers.New(&fakeStore{}, nil))
	b.Cleanup(srv.Close)
	body := []byte(`{"text":"замер производительности"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Post(srv.URL+"/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatalf("POST /messages: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkGRPCCreateMessage(b *testing.B) {
	quietLogs(b)
	client := newBenchGRPCClient(b, &fakeStore{})
	req := &grpcapi.CreateMessageRequest{Text: "замер производительности"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.CreateMessage(context.Background(), req); err != nil {
			b.Fatalf("CreateMessage: %v", err)
		}
	}
}

func BenchmarkHTTPListMessages(b *testing.B) {
	quietLogs(b)
	store := &fakeStore{}
	for i := 0; i < 100; i++ {
		_, _ = store.Create(context.Background(), "сообщение для замера списка")
	}
	srv := httptest.NewServer(handlers.New(store, nil))
	b.Cleanup(srv.Close)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(srv.URL + "/messages")
		if err != nil {
			b.Fatalf("GET /messages: %v", err)
		}
		var msgs []handlers.Message
		if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
			b.Fatalf("декодирование: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func BenchmarkGRPCListMessages(b *testing.B) {
	quietLogs(b)
	store := &fakeStore{}
	for i := 0; i < 100; i++ {
		_, _ = store.Create(context.Background(), "сообщение для замера списка")
	}
	client := newBenchGRPCClient(b, store)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stream, err := client.ListMessages(context.Background(), &grpcapi.ListMessagesRequest{})
		if err != nil {
			b.Fatalf("ListMessages: %v", err)
		}
		for {
			if _, err := stream.Recv(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				b.Fatalf("стрим оборвался: %v", err)
			}
		}
	}
}
