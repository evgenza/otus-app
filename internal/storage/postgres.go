package storage

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/evgenza/otus-app/internal/domain/messaging"
)

type Postgres struct {
	pool    *pgxpool.Pool
	targets []string
}

func New(ctx context.Context, dsn string, targets ...string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	p := &Postgres{pool: pool, targets: targets}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	// Блокировка в самой БД защищает одновременный старт реплик и без etcd.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(71820260907)`); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS messages (
		id         BIGSERIAL PRIMARY KEY,
		text       TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS text_hash TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE messages SET text_hash = encode(sha256(convert_to(text, 'UTF8')), 'hex') WHERE text_hash = ''`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS idem_key TEXT`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS messages_idem_key_idx ON messages (idem_key)`)
	if err != nil {
		return err
	}
	if err := p.migrateOutbox(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Create(ctx context.Context, text, idemKey string) (messaging.Message, error) {
	var m messaging.Message
	if err := messaging.ValidateText(text); err != nil {
		return m, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return m, err
	}
	defer rollback(ctx, tx)
	err = tx.QueryRow(ctx,
		`INSERT INTO messages (text, text_hash, idem_key) VALUES ($1, $2, NULLIF($3, ''))
   ON CONFLICT (idem_key) DO NOTHING RETURNING id, text, text_hash, created_at`,
		text, messaging.Checksum(text), idemKey).Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id, text, text_hash, created_at FROM messages WHERE idem_key=$1`, idemKey).
			Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt)
		if err != nil {
			return m, err
		}
		if m.Text != text {
			return messaging.Message{}, messaging.ErrIdempotencyConflict
		}
		m.ChecksumOK = m.Checksum == messaging.Checksum(m.Text)
		return m, tx.Commit(ctx)
	}
	if err != nil {
		return m, err
	}
	m.ChecksumOK = true
	payload, err := json.Marshal(m.CreatedEvent())
	if err != nil {
		return m, err
	}
	for _, target := range p.targets {
		if _, err = tx.Exec(ctx, `INSERT INTO message_outbox (message_id, target, payload)
   VALUES ($1, $2, $3) ON CONFLICT (message_id, target) DO NOTHING`, m.ID, target, payload); err != nil {
			return messaging.Message{}, err
		}
	}
	return m, tx.Commit(ctx)
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) List(ctx context.Context) ([]messaging.Message, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, text, text_hash, created_at FROM messages ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := make([]messaging.Message, 0)
	for rows.Next() {
		var m messaging.Message
		if err := rows.Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ChecksumOK = m.Checksum == messaging.Checksum(m.Text)
		if !m.ChecksumOK {
			slog.WarnContext(ctx, "контрольная сумма сообщения не совпадает", "id", m.ID)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Search ищет подстроку в тексте через ILIKE. Индекс тут не работает:
// шаблон с ведущим процентом заставляет планировщик читать всю таблицу
func (p *Postgres) Search(ctx context.Context, query string, limit int) ([]messaging.Message, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, text, text_hash, created_at FROM messages
		 WHERE text ILIKE '%' || $1 || '%' ORDER BY id DESC LIMIT $2`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := make([]messaging.Message, 0)
	for rows.Next() {
		var m messaging.Message
		if err := rows.Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ChecksumOK = m.Checksum == messaging.Checksum(m.Text)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (p *Postgres) Close() {
	p.pool.Close()
}

func rollback(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
