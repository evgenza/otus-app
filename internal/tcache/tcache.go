package tcache

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/tarantool/go-tarantool/v2"
	"github.com/tarantool/go-tarantool/v2/pool"
)

type Cache struct {
	pool *pool.ConnectionPool
	ttl  time.Duration
}

func New(ctx context.Context) (*Cache, error) {
	addrs := os.Getenv("TARANTOOL_ADDRS")
	if addrs == "" {
		return nil, nil
	}
	user := os.Getenv("TARANTOOL_USER")
	if user == "" {
		user = "app"
	}
	password := os.Getenv("TARANTOOL_PASSWORD")
	if password == "" {
		password = "app-secret"
	}
	instances := make([]pool.Instance, 0)
	for _, addr := range strings.Split(addrs, ",") {
		instances = append(instances, pool.Instance{
			Name: addr,
			Dialer: tarantool.NetDialer{
				Address:  addr,
				User:     user,
				Password: password,
			},
			Opts: tarantool.Opts{Timeout: 3 * time.Second},
		})
	}
	connCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := pool.Connect(connCtx, instances)
	if err != nil {
		return nil, err
	}
	slog.Info("кэш в Tarantool подключен", "instances", len(instances))
	return &Cache{pool: p, ttl: 10 * time.Second}, nil
}

func (c *Cache) Get(ctx context.Context, key string) (string, bool) {
	if c == nil {
		return "", false
	}
	resp, err := c.pool.Do(
		tarantool.NewSelectRequest("cache").Context(ctx).Iterator(tarantool.IterEq).Key([]any{key}),
		pool.ANY,
	).Get()
	if err != nil {
		slog.WarnContext(ctx, "кэш недоступен на чтение", "err", err)
		return "", false
	}
	if len(resp) == 0 {
		return "", false
	}
	tuple, ok := resp[0].([]any)
	if !ok || len(tuple) < 3 {
		return "", false
	}
	value, ok := tuple[1].(string)
	if !ok {
		return "", false
	}
	expires, ok := toInt64(tuple[2])
	if !ok || time.Now().Unix() > expires {
		return "", false
	}
	return value, true
}

func (c *Cache) Set(ctx context.Context, key, value string) {
	if c == nil {
		return
	}
	expires := time.Now().Add(c.ttl).Unix()
	_, err := c.pool.Do(
		tarantool.NewReplaceRequest("cache").Context(ctx).Tuple([]any{key, value, expires}),
		pool.RW,
	).Get()
	if err != nil {
		slog.WarnContext(ctx, "кэш недоступен на запись", "err", err)
	}
}

func (c *Cache) Delete(ctx context.Context, key string) {
	if c == nil {
		return
	}
	_, err := c.pool.Do(
		tarantool.NewDeleteRequest("cache").Context(ctx).Key([]any{key}),
		pool.RW,
	).Get()
	if err != nil {
		slog.WarnContext(ctx, "кэш недоступен на удаление", "err", err)
	}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case uint32:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}
