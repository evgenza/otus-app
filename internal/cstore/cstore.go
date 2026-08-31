// Package cstore хранит поток событий в Cassandra: широкие партиции по
// дням для ленты и отдельная таблица под выборку по идентификатору.
package cstore

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"github.com/evgenza/otus-app/internal/broker"
	"github.com/evgenza/otus-app/internal/observability"
)

// ErrNotConfigured возвращается, когда Cassandra не настроена окружением.
var ErrNotConfigured = errors.New("cassandra не настроена")

// Event — событие в том виде, в котором оно лежит в Cassandra.
type Event struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Checksum  string    `json:"checksum"`
	Producer  string    `json:"producer,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	session *gocql.Session
	ks      string
}

// New поднимает keyspace и таблицы и возвращает сессию с кворумной
// консистентностью. Пустой CASSANDRA_HOSTS означает, что интеграция
// выключена.
func New(_ context.Context) (*Store, error) {
	hosts := envList("CASSANDRA_HOSTS")
	if len(hosts) == 0 {
		return nil, nil
	}
	ks := envOr("CASSANDRA_KEYSPACE", "otus")
	rf, _ := strconv.Atoi(envOr("CASSANDRA_RF", "3"))

	// Первая сессия — без keyspace, только чтобы его создать.
	bootstrap := newCluster(hosts, "")
	adminSession, err := bootstrap.CreateSession()
	if err != nil {
		return nil, err
	}
	err = adminSession.Query(`CREATE KEYSPACE IF NOT EXISTS ` + ks +
		` WITH replication = {'class': 'SimpleStrategy', 'replication_factor': ` +
		strconv.Itoa(rf) + `}`).Exec()
	adminSession.Close()
	if err != nil {
		return nil, err
	}

	session, err := newCluster(hosts, ks).CreateSession()
	if err != nil {
		return nil, err
	}
	store := &Store{session: session, ks: ks}
	if err := store.migrate(); err != nil {
		session.Close()
		return nil, err
	}
	slog.Info("Cassandra подключена", "hosts", hosts, "keyspace", ks, "rf", rf)
	return store, nil
}

func newCluster(hosts []string, keyspace string) *gocql.ClusterConfig {
	cluster := gocql.NewCluster(hosts...)
	cluster.Keyspace = keyspace
	cluster.ProtoVersion = 4
	cluster.Timeout = 5 * time.Second
	cluster.ConnectTimeout = 5 * time.Second
	// QUORUM: запись подтверждают 2 узла из 3, поэтому потеря одного узла
	// не теряет данные и не блокирует запись.
	cluster.Consistency = consistency()
	// Токен-ориентированная политика шлет запрос сразу на узел-владелец
	// партиции, а список живых узлов драйвер обновляет сам.
	cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(gocql.RoundRobinHostPolicy())
	cluster.RetryPolicy = &gocql.ExponentialBackoffRetryPolicy{NumRetries: 3, Min: 50 * time.Millisecond, Max: time.Second}
	cluster.ReconnectInterval = time.Second
	return cluster
}

func consistency() gocql.Consistency {
	if raw := strings.TrimSpace(os.Getenv("CASSANDRA_CONSISTENCY")); raw != "" {
		if c, err := gocql.ParseConsistencyWrapper(raw); err == nil {
			return c
		}
	}
	return gocql.Quorum
}

func (s *Store) migrate() error {
	statements := []string{
		// Партиция на сутки, внутри — сортировка по времени: типовая
		// раскладка ленты событий под запрос "последние N за день".
		`CREATE TABLE IF NOT EXISTS message_events (
			day        text,
			created_at timestamp,
			id         bigint,
			text       text,
			checksum   text,
			producer   text,
			PRIMARY KEY ((day), created_at, id)
		) WITH CLUSTERING ORDER BY (created_at DESC, id DESC)`,
		// Дубль тех же данных под другой запрос: в Cassandra таблица
		// проектируется под запрос, а не наоборот.
		`CREATE TABLE IF NOT EXISTS events_by_id (
			id         bigint PRIMARY KEY,
			text       text,
			checksum   text,
			producer   text,
			created_at timestamp
		)`,
	}
	for _, stmt := range statements {
		if err := s.session.Query(stmt).Exec(); err != nil {
			return err
		}
	}
	return nil
}

// Save пишет событие в обе таблицы. Батч логированный: обе записи либо
// применятся, либо нет, иначе выборка по id рассинхронизируется с лентой.
func (s *Store) Save(ctx context.Context, ev broker.Event, producer string) error {
	if s == nil {
		return ErrNotConfigured
	}
	start := time.Now()
	day := ev.CreatedAt.UTC().Format("2006-01-02")
	batch := s.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query(`INSERT INTO message_events (day, created_at, id, text, checksum, producer)
		VALUES (?, ?, ?, ?, ?, ?)`, day, ev.CreatedAt, ev.ID, ev.Text, ev.Checksum, producer)
	batch.Query(`INSERT INTO events_by_id (id, text, checksum, producer, created_at)
		VALUES (?, ?, ?, ?, ?)`, ev.ID, ev.Text, ev.Checksum, producer, ev.CreatedAt)
	err := s.session.ExecuteBatch(batch)
	observability.ObserveStorage("cassandra", "save", start, err)
	return err
}

// Recent отдает последние события за сутки: один запрос в одну партицию.
func (s *Store) Recent(ctx context.Context, day string, limit int) ([]Event, error) {
	if s == nil {
		return nil, ErrNotConfigured
	}
	if day == "" {
		day = time.Now().UTC().Format("2006-01-02")
	}
	start := time.Now()
	iter := s.session.Query(
		`SELECT id, text, checksum, producer, created_at FROM message_events WHERE day = ? LIMIT ?`,
		day, limit).WithContext(ctx).Iter()
	events := scan(iter)
	err := iter.Close()
	observability.ObserveStorage("cassandra", "recent", start, err)
	return events, err
}

// ByID читает событие по ключу из денормализованной таблицы.
func (s *Store) ByID(ctx context.Context, id int64) (Event, error) {
	if s == nil {
		return Event{}, ErrNotConfigured
	}
	start := time.Now()
	var ev Event
	err := s.session.Query(
		`SELECT id, text, checksum, producer, created_at FROM events_by_id WHERE id = ?`, id).
		WithContext(ctx).Scan(&ev.ID, &ev.Text, &ev.Checksum, &ev.Producer, &ev.CreatedAt)
	observability.ObserveStorage("cassandra", "by_id", start, err)
	return ev, err
}

// Scan ищет подстроку в тексте перебором таблицы: у Cassandra нет
// полнотекстового поиска, и это честная цена запроса не по ключу.
// Используется для сравнения с Elasticsearch.
func (s *Store) Scan(ctx context.Context, substr string, limit int) ([]Event, int, error) {
	if s == nil {
		return nil, 0, ErrNotConfigured
	}
	start := time.Now()
	iter := s.session.Query(`SELECT id, text, checksum, producer, created_at FROM events_by_id`).
		WithContext(ctx).PageSize(1000).Iter()

	found := make([]Event, 0)
	scanned := 0
	var ev Event
	needle := strings.ToLower(substr)
	for iter.Scan(&ev.ID, &ev.Text, &ev.Checksum, &ev.Producer, &ev.CreatedAt) {
		scanned++
		if len(found) < limit && strings.Contains(strings.ToLower(ev.Text), needle) {
			found = append(found, ev)
		}
	}
	err := iter.Close()
	observability.ObserveStorage("cassandra", "scan", start, err)
	return found, scanned, err
}

// Count считает строки в таблице событий.
func (s *Store) Count(ctx context.Context) (int64, error) {
	if s == nil {
		return 0, ErrNotConfigured
	}
	var n int64
	err := s.session.Query(`SELECT count(*) FROM events_by_id`).WithContext(ctx).Scan(&n)
	return n, err
}

// Close закрывает сессию.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.session.Close()
}

func scan(iter *gocql.Iter) []Event {
	events := make([]Event, 0)
	var ev Event
	for iter.Scan(&ev.ID, &ev.Text, &ev.Checksum, &ev.Producer, &ev.CreatedAt) {
		events = append(events, ev)
	}
	return events
}

func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
