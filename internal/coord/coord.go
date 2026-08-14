package coord

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

type Coordinator struct {
	client *clientv3.Client
}

func New() (*Coordinator, error) {
	endpoints := os.Getenv("ETCD_ENDPOINTS")
	if endpoints == "" {
		return nil, nil
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(endpoints, ","),
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &Coordinator{client: client}, nil
}

func (c *Coordinator) Close() {
	if c == nil {
		return
	}
	_ = c.client.Close()
}

func (c *Coordinator) WithLock(ctx context.Context, name string, fn func() error) error {
	if c == nil {
		return fn()
	}
	session, err := concurrency.NewSession(c.client, concurrency.WithTTL(15), concurrency.WithContext(ctx))
	if err != nil {
		slog.Warn("etcd недоступен, работаю без распределенной блокировки", "err", err)
		return fn()
	}
	defer func() { _ = session.Close() }()

	mutex := concurrency.NewMutex(session, name)
	lockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := mutex.Lock(lockCtx); err != nil {
		return err
	}
	slog.Info("распределенная блокировка получена", "name", name)
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		if err := mutex.Unlock(unlockCtx); err != nil {
			slog.Warn("не удалось снять блокировку, ее освободит истекший lease", "name", name, "err", err)
		} else {
			slog.Info("распределенная блокировка снята", "name", name)
		}
	}()
	return fn()
}

func (c *Coordinator) WatchInt(ctx context.Context, key string, apply func(int64)) {
	if c == nil {
		return
	}
	rev := c.fetchInt(ctx, key, apply)

	go func() {
		for {
			opts := []clientv3.OpOption{}
			if rev > 0 {
				opts = append(opts, clientv3.WithRev(rev+1))
			}
			for watchResp := range c.client.Watch(clientv3.WithRequireLeader(ctx), key, opts...) {
				if err := watchResp.Err(); err != nil {
					slog.Warn("подписка на конфигурацию прервана", "key", key, "err", err)
					continue
				}
				for _, ev := range watchResp.Events {
					if ev.Type == clientv3.EventTypePut {
						applyInt(key, string(ev.Kv.Value), apply)
					}
				}
				if watchResp.Header.Revision > rev {
					rev = watchResp.Header.Revision
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			slog.Info("переподключаю подписку на конфигурацию", "key", key)
			rev = c.fetchInt(ctx, key, apply)
		}
	}()
}

func (c *Coordinator) fetchInt(ctx context.Context, key string, apply func(int64)) int64 {
	getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.client.Get(getCtx, key)
	if err != nil {
		slog.Warn("не удалось прочитать конфигурацию из etcd", "key", key, "err", err)
		return 0
	}
	if len(resp.Kvs) > 0 {
		applyInt(key, string(resp.Kvs[0].Value), apply)
	}
	return resp.Header.Revision
}

func applyInt(key, raw string, apply func(int64)) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		slog.Warn("некорректное значение конфигурации в etcd", "key", key, "value", raw)
		return
	}
	slog.Info("получено значение конфигурации из etcd", "key", key, "value", n)
	apply(n)
}
