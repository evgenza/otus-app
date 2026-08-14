package storage

import (
	"context"
	"errors"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/security"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*Postgres, error) {
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
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) migrate(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS messages (
		id         BIGSERIAL PRIMARY KEY,
		text       TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS text_hash TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`UPDATE messages SET text_hash = encode(sha256(convert_to(text, 'UTF8')), 'hex') WHERE text_hash = ''`)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS idem_key TEXT`)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS messages_idem_key_idx ON messages (idem_key)`)
	return err
}

func (p *Postgres) Create(ctx context.Context, text, idemKey string) (handlers.Message, error) {
	var m handlers.Message
	if idemKey == "" {
		err := p.pool.QueryRow(ctx,
			`INSERT INTO messages (text, text_hash) VALUES ($1, $2) RETURNING id, text, text_hash, created_at`,
			text, security.Checksum(text)).
			Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt)
		m.ChecksumOK = true
		return m, err
	}

	err := p.pool.QueryRow(ctx,
		`INSERT INTO messages (text, text_hash, idem_key) VALUES ($1, $2, $3)
		 ON CONFLICT (idem_key) DO NOTHING
		 RETURNING id, text, text_hash, created_at`,
		text, security.Checksum(text), idemKey).
		Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.InfoContext(ctx, "повторный запрос с тем же ключом идемпотентности", "idem_key", idemKey)
		err = p.pool.QueryRow(ctx,
			`SELECT id, text, text_hash, created_at FROM messages WHERE idem_key = $1`, idemKey).
			Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt)
	}
	m.ChecksumOK = m.Checksum == security.Checksum(m.Text)
	return m, err
}

func (p *Postgres) List(ctx context.Context) ([]handlers.Message, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, text, text_hash, created_at FROM messages ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := make([]handlers.Message, 0)
	for rows.Next() {
		var m handlers.Message
		if err := rows.Scan(&m.ID, &m.Text, &m.Checksum, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ChecksumOK = m.Checksum == security.Checksum(m.Text)
		if !m.ChecksumOK {
			slog.WarnContext(ctx, "контрольная сумма сообщения не совпадает", "id", m.ID)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (p *Postgres) Close() {
	p.pool.Close()
}
