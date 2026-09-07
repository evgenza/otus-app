//go:build integration

package ratelimit

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type lostReplyConn struct {
	net.Conn
	drop *atomic.Bool
}

func (c *lostReplyConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	if n > 0 && c.drop.Swap(false) {
		_ = c.Conn.Close()
		return 0, io.EOF
	}
	return n, err
}

func TestRedisLostReplyDoesNotRepeatIncrement(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR не задан")
	}
	t.Setenv("VALKEY_ADDRS", addr)
	t.Setenv("VALKEY_MASTER_NAME", "")
	limiter := New()
	counter := limiter.counter.(*valkeyCounter)
	client := counter.client.(*redis.Client)
	t.Cleanup(func() { _ = client.Close() })
	var drop atomic.Bool
	dial := client.Options().Dialer
	client.Options().Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &lostReplyConn{Conn: conn, drop: &drop}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := "test:lost-reply:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	admin := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = admin.Del(context.Background(), key, key+":warm").Err(); _ = admin.Close() })
	if _, err := counter.Hit(ctx, key+":warm", time.Minute); err != nil {
		t.Fatal(err)
	}
	drop.Store(true)
	if _, err := counter.Hit(ctx, key, time.Minute); err == nil {
		t.Fatal("ожидалась потеря ответа Redis")
	}
	count, err := admin.Get(ctx, key).Int64()
	if err != nil || count != 1 {
		t.Fatalf("после потери ответа счетчик %d: %v", count, err)
	}
	if ttl := admin.PTTL(ctx, key).Val(); ttl <= 0 {
		t.Fatal("счетчик потерял TTL")
	}
	// Следующий логический запрос восстанавливает соединение и учитывается один раз.
	count, err = counter.Hit(ctx, key, time.Minute)
	if err != nil || count != 2 {
		t.Fatalf("после восстановления счетчик %d: %v", count, err)
	}
}
