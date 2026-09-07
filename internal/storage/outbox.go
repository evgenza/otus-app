package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/evgenza/otus-app/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (p *Postgres) migrateOutbox(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS message_outbox (
	 message_id BIGINT NOT NULL REFERENCES messages(id),
	 target TEXT NOT NULL CHECK (target IN ('kafka', 'rabbitmq', 'nats')),
	 payload JSONB NOT NULL,
	 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	 available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	 lease_token TEXT,
	 attempts INTEGER NOT NULL DEFAULT 0,
	 delivered_at TIMESTAMPTZ,
	 PRIMARY KEY (message_id, target)
	);
	CREATE INDEX IF NOT EXISTS message_outbox_pending_idx
	 ON message_outbox(target, available_at, message_id) WHERE delivered_at IS NULL;`)
	return err
}

// Claim кратковременно блокирует строку и фиксирует аренду до сетевого вызова.
// Новый токен защищает от завершения задания воркером с устаревшей арендой.
func (p *Postgres) Claim(ctx context.Context, target string, lease time.Duration) (*outbox.Delivery, error) {
	d := &outbox.Delivery{Target: target, Token: uuid.NewString()}
	var payload []byte
	err := p.pool.QueryRow(ctx, `WITH candidate AS (
	 SELECT message_id, target FROM message_outbox
	 WHERE target=$1 AND delivered_at IS NULL AND available_at <= now()
	 ORDER BY available_at, message_id FOR UPDATE SKIP LOCKED LIMIT 1
	) UPDATE message_outbox o SET lease_token=$2,
	 available_at=now()+($3 * interval '1 millisecond'), attempts=o.attempts+1
	 FROM candidate c WHERE o.message_id=c.message_id AND o.target=c.target
	 RETURNING o.message_id, o.payload, o.attempts`, target, d.Token, lease.Milliseconds()).Scan(&d.MessageID, &payload, &d.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &d.Event); err != nil {
		return nil, err
	}
	return d, nil
}

func (p *Postgres) Complete(ctx context.Context, d outbox.Delivery) error {
	tag, err := p.pool.Exec(ctx, `UPDATE message_outbox SET delivered_at=now(), lease_token=NULL
	 WHERE message_id=$1 AND target=$2 AND lease_token=$3 AND delivered_at IS NULL`, d.MessageID, d.Target, d.Token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return outbox.ErrLeaseLost
	}
	return nil
}

func (p *Postgres) Retry(ctx context.Context, d outbox.Delivery, delay time.Duration) error {
	tag, err := p.pool.Exec(ctx, `UPDATE message_outbox SET lease_token=NULL,
	 available_at=now()+($4 * interval '1 millisecond')
	 WHERE message_id=$1 AND target=$2 AND lease_token=$3 AND delivered_at IS NULL`, d.MessageID, d.Target, d.Token, delay.Milliseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return outbox.ErrLeaseLost
	}
	return nil
}

func (p *Postgres) OutboxStats(ctx context.Context, target string) (outbox.Stats, error) {
	var s outbox.Stats
	err := p.pool.QueryRow(ctx, `SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now()-min(created_at)),0)::float8
	 FROM message_outbox WHERE target=$1 AND delivered_at IS NULL`, target).Scan(&s.Pending, &s.OldestSeconds)
	return s, err
}
