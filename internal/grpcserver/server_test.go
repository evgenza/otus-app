package grpcserver_test

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
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/evgenza/otus-app/internal/grpcapi"
	"github.com/evgenza/otus-app/internal/grpcserver"
	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/security"
)

type fakeStore struct {
	items   []handlers.Message
	failAll bool
}

func (f *fakeStore) Create(_ context.Context, text string) (handlers.Message, error) {
	if f.failAll {
		return handlers.Message{}, errors.New("сбой хранилища")
	}
	m := handlers.Message{
		ID:         int64(len(f.items) + 1),
		Text:       text,
		Checksum:   security.Checksum(text),
		ChecksumOK: true,
		CreatedAt:  time.Now(),
	}
	f.items = append(f.items, m)
	return m, nil
}

func (f *fakeStore) List(_ context.Context) ([]handlers.Message, error) {
	if f.failAll {
		return nil, errors.New("сбой хранилища")
	}
	return f.items, nil
}

func newClient(t *testing.T, store handlers.MessageStore, auth *security.Auth) grpcapi.MessageServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpcserver.New(store, auth, nil)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

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
	return grpcapi.NewMessageServiceClient(conn)
}

func TestCreateMessageUnary(t *testing.T) {
	store := &fakeStore{}
	client := newClient(t, store, nil)

	msg, err := client.CreateMessage(context.Background(), &grpcapi.CreateMessageRequest{Text: "привет"})
	if err != nil {
		t.Fatalf("CreateMessage вернул ошибку: %v", err)
	}
	if msg.GetId() == 0 || msg.GetText() != "привет" || !msg.GetChecksumOk() {
		t.Fatalf("неожиданное сообщение: %+v", msg)
	}
	if msg.GetChecksum() != security.Checksum("привет") {
		t.Fatal("контрольная сумма не совпадает")
	}
}

func TestCreateMessageEmptyText(t *testing.T) {
	client := newClient(t, &fakeStore{}, nil)

	_, err := client.CreateMessage(context.Background(), &grpcapi.CreateMessageRequest{Text: "  "})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ожидался InvalidArgument, получено: %v", err)
	}
}

func TestListMessagesServerStream(t *testing.T) {
	store := &fakeStore{}
	for _, text := range []string{"первое", "второе", "третье"} {
		_, _ = store.Create(context.Background(), text)
	}
	client := newClient(t, store, nil)

	stream, err := client.ListMessages(context.Background(), &grpcapi.ListMessagesRequest{})
	if err != nil {
		t.Fatalf("ListMessages вернул ошибку: %v", err)
	}
	var got []string
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("стрим оборвался: %v", err)
		}
		got = append(got, msg.GetText())
	}
	if len(got) != 3 {
		t.Fatalf("ожидалось 3 сообщения из стрима, получено %d", len(got))
	}
}

func TestListMessagesLimit(t *testing.T) {
	store := &fakeStore{}
	for _, text := range []string{"первое", "второе", "третье"} {
		_, _ = store.Create(context.Background(), text)
	}
	client := newClient(t, store, nil)

	stream, err := client.ListMessages(context.Background(), &grpcapi.ListMessagesRequest{Limit: 2})
	if err != nil {
		t.Fatalf("ListMessages вернул ошибку: %v", err)
	}
	count := 0
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
		count++
	}
	if count != 2 {
		t.Fatalf("ожидалось 2 сообщения при limit=2, получено %d", count)
	}
}

func TestBatchCreateClientStream(t *testing.T) {
	store := &fakeStore{}
	client := newClient(t, store, nil)

	stream, err := client.BatchCreate(context.Background())
	if err != nil {
		t.Fatalf("BatchCreate вернул ошибку: %v", err)
	}
	for _, text := range []string{"раз", "два", "три"} {
		if err := stream.Send(&grpcapi.CreateMessageRequest{Text: text}); err != nil {
			t.Fatalf("отправка в поток не удалась: %v", err)
		}
	}
	summary, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv вернул ошибку: %v", err)
	}
	if summary.GetCreated() != 3 || len(summary.GetIds()) != 3 {
		t.Fatalf("неожиданная сводка: %+v", summary)
	}
	if len(store.items) != 3 {
		t.Fatalf("ожидалось 3 сообщения в хранилище, получено %d", len(store.items))
	}
}

func TestChatBidiStream(t *testing.T) {
	client := newClient(t, &fakeStore{}, nil)

	stream, err := client.Chat(context.Background())
	if err != nil {
		t.Fatalf("Chat вернул ошибку: %v", err)
	}
	for _, note := range []string{"привет", "как дела"} {
		if err := stream.Send(&grpcapi.ChatNote{Text: note}); err != nil {
			t.Fatalf("отправка в поток не удалась: %v", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			t.Fatalf("приём из потока не удался: %v", err)
		}
		if reply.GetText() != "эхо: "+note {
			t.Fatalf("неожиданный ответ: %q", reply.GetText())
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("закрытие потока не удалось: %v", err)
	}
}

func newTestAuth(t *testing.T) (*security.Auth, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("не удалось сгенерировать ключ: %v", err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"kid": "test-key",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))
	t.Cleanup(jwks.Close)

	t.Setenv("AUTH_JWKS_URL", jwks.URL)
	t.Setenv("AUTH_ISSUER", "test-issuer")
	return security.NewAuth(), key
}

func signToken(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "test-issuer",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("не удалось подписать токен: %v", err)
	}
	return raw
}

func TestCreateMessageRequiresToken(t *testing.T) {
	auth, _ := newTestAuth(t)
	client := newClient(t, &fakeStore{}, auth)

	_, err := client.CreateMessage(context.Background(), &grpcapi.CreateMessageRequest{Text: "без токена"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ожидался Unauthenticated, получено: %v", err)
	}
}

func TestBatchCreateRequiresToken(t *testing.T) {
	auth, _ := newTestAuth(t)
	client := newClient(t, &fakeStore{}, auth)

	stream, err := client.BatchCreate(context.Background())
	if err != nil {
		t.Fatalf("BatchCreate вернул ошибку: %v", err)
	}
	_ = stream.Send(&grpcapi.CreateMessageRequest{Text: "без токена"})
	_, err = stream.CloseAndRecv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ожидался Unauthenticated, получено: %v", err)
	}
}

func TestCreateMessageWithValidToken(t *testing.T) {
	auth, key := newTestAuth(t)
	client := newClient(t, &fakeStore{}, auth)

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer "+signToken(t, key))
	msg, err := client.CreateMessage(ctx, &grpcapi.CreateMessageRequest{Text: "с токеном"})
	if err != nil {
		t.Fatalf("CreateMessage с валидным токеном вернул ошибку: %v", err)
	}
	if msg.GetText() != "с токеном" {
		t.Fatalf("неожиданное сообщение: %+v", msg)
	}
}

func TestCreateMessageForgedToken(t *testing.T) {
	auth, _ := newTestAuth(t)
	client := newClient(t, &fakeStore{}, auth)

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer forged.token.value")
	_, err := client.CreateMessage(ctx, &grpcapi.CreateMessageRequest{Text: "взлом"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ожидался Unauthenticated, получено: %v", err)
	}
}

func TestListMessagesOpenWithoutToken(t *testing.T) {
	auth, _ := newTestAuth(t)
	store := &fakeStore{}
	_, _ = store.Create(context.Background(), "публичное")
	client := newClient(t, store, auth)

	stream, err := client.ListMessages(context.Background(), &grpcapi.ListMessagesRequest{})
	if err != nil {
		t.Fatalf("ListMessages вернул ошибку: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("чтение без токена должно быть доступно: %v", err)
	}
}
