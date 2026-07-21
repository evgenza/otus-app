//go:build integration

package storage_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/evgenza/otus-app/internal/grpcapi"
	"github.com/evgenza/otus-app/internal/grpcserver"
	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/security"
	"github.com/evgenza/otus-app/internal/storage"
)

func newStore(t *testing.T) *storage.Postgres {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL не задан — интеграционный тест пропущен")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := storage.New(ctx, dsn)
	if err != nil {
		t.Fatalf("не удалось подключиться к базе: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestStorageCreateAndList(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "интеграционное сообщение")
	if err != nil {
		t.Fatalf("Create вернул ошибку: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("ожидался ненулевой ID")
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List вернул ошибку: %v", err)
	}
	found := false
	for _, m := range list {
		if m.ID == created.ID && m.Text == "интеграционное сообщение" {
			found = true
			if !m.ChecksumOK {
				t.Error("контрольная сумма нетронутого сообщения должна сходиться")
			}
		}
	}
	if !found {
		t.Fatal("созданное сообщение не найдено в списке")
	}
}

func TestChecksumDetectsTampering(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "неизменное сообщение")
	if err != nil {
		t.Fatalf("Create вернул ошибку: %v", err)
	}

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("не удалось подключиться к базе: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET text = 'подменённый текст' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("не удалось подменить текст: %v", err)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List вернул ошибку: %v", err)
	}
	for _, m := range list {
		if m.ID == created.ID {
			if m.ChecksumOK {
				t.Error("подмена текста не обнаружена по контрольной сумме")
			}
			return
		}
	}
	t.Fatal("сообщение не найдено в списке")
}

func TestAPIWithDatabase(t *testing.T) {
	store := newStore(t)
	srv := httptest.NewServer(handlers.New(store, nil))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/messages", "application/json",
		strings.NewReader(`{"text":"e2e через api"}`))
	if err != nil {
		t.Fatalf("POST /messages: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ожидался статус 201, получен %d", resp.StatusCode)
	}

	listResp, err := http.Get(srv.URL + "/messages")
	if err != nil {
		t.Fatalf("GET /messages: %v", err)
	}
	defer func() { _ = listResp.Body.Close() }()

	var msgs []handlers.Message
	if err := json.NewDecoder(listResp.Body).Decode(&msgs); err != nil {
		t.Fatalf("декодирование списка: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("ожидалось хотя бы одно сообщение после POST")
	}
}

func newAuthWithKey(t *testing.T) (*security.Auth, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("не удалось сгенерировать ключ: %v", err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"kid": "integration-key",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))
	t.Cleanup(jwks.Close)
	t.Setenv("AUTH_JWKS_URL", jwks.URL)
	t.Setenv("AUTH_ISSUER", "integration-issuer")
	return security.NewAuth(), key
}

func TestAPIAccessControlWithDatabase(t *testing.T) {
	store := newStore(t)
	auth, key := newAuthWithKey(t)
	srv := httptest.NewServer(handlers.New(store, auth))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/messages", "application/json",
		strings.NewReader(`{"text":"без токена"}`))
	if err != nil {
		t.Fatalf("POST /messages: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("без токена ожидался статус 401, получен %d", resp.StatusCode)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "integration-issuer",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "integration-key"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("не удалось подписать токен: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/messages",
		strings.NewReader(`{"text":"с токеном через api"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	authResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /messages с токеном: %v", err)
	}
	defer func() { _ = authResp.Body.Close() }()
	if authResp.StatusCode != http.StatusCreated {
		t.Fatalf("с токеном ожидался статус 201, получен %d", authResp.StatusCode)
	}
}

func TestGRPCWithDatabase(t *testing.T) {
	store := newStore(t)
	lis := bufconn.Listen(1 << 20)
	gsrv := grpcserver.New(store, nil, nil)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("не удалось подключиться к bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcapi.NewMessageServiceClient(conn)
	ctx := context.Background()

	batch, err := client.BatchCreate(ctx)
	if err != nil {
		t.Fatalf("BatchCreate вернул ошибку: %v", err)
	}
	for _, text := range []string{"grpc-интеграция раз", "grpc-интеграция два"} {
		if err := batch.Send(&grpcapi.CreateMessageRequest{Text: text}); err != nil {
			t.Fatalf("отправка в поток не удалась: %v", err)
		}
	}
	summary, err := batch.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv вернул ошибку: %v", err)
	}
	if summary.GetCreated() != 2 {
		t.Fatalf("ожидалось 2 созданных сообщения, получено %d", summary.GetCreated())
	}

	stream, err := client.ListMessages(ctx, &grpcapi.ListMessagesRequest{})
	if err != nil {
		t.Fatalf("ListMessages вернул ошибку: %v", err)
	}
	found := 0
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("стрим оборвался: %v", err)
		}
		if strings.HasPrefix(msg.GetText(), "grpc-интеграция") {
			if !msg.GetChecksumOk() {
				t.Error("контрольная сумма сообщения из gRPC-стрима не сходится")
			}
			found++
		}
	}
	if found < 2 {
		t.Fatalf("в стриме найдено %d сообщений из 2", found)
	}
}
