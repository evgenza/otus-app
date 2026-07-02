package storage

import (
	"context"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/evgenza/otus-app/internal/handlers"
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
	return err
}

func (p *Postgres) Create(ctx context.Context, text string) (handlers.Message, error) {
	var m handlers.Message
	err := p.pool.QueryRow(ctx,
		`INSERT INTO messages (text) VALUES ($1) RETURNING id, text, created_at`, text).
		Scan(&m.ID, &m.Text, &m.CreatedAt)
	return m, err
}

func (p *Postgres) List(ctx context.Context) ([]handlers.Message, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, text, created_at FROM messages ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := make([]handlers.Message, 0)
	for rows.Next() {
		var m handlers.Message
		if err := rows.Scan(&m.ID, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (p *Postgres) Close() {
	p.pool.Close()
}
