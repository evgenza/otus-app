// Package hdfsstore пишет и читает большие файлы в HDFS: файл режется на
// блоки и раскладывается по датанодам с заданной репликацией, поэтому
// потеря датаноды не мешает чтению.
package hdfsstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/colinmarc/hdfs/v2"

	"github.com/evgenza/otus-app/internal/observability"
)

// ErrNotConfigured возвращается, когда HDFS не настроен переменными окружения.
var ErrNotConfigured = errors.New("HDFS не настроен")

// File — метаданные файла в HDFS.
type File struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified,omitzero"`
}

type Store struct {
	client      *hdfs.Client
	dir         string
	replication int
	blockSize   int64
}

// New подключается к namenode и создает рабочий каталог. Пустой
// HDFS_NAMENODES означает, что интеграция выключена.
func New(_ context.Context) (*Store, error) {
	addrs := envList("HDFS_NAMENODES")
	if len(addrs) == 0 {
		return nil, nil
	}
	user := envOr("HDFS_USER", "root")
	client, err := hdfs.NewClient(hdfs.ClientOptions{
		Addresses: addrs,
		User:      user,
	})
	if err != nil {
		return nil, err
	}
	replication, _ := strconv.Atoi(envOr("HDFS_REPLICATION", "3"))
	blockMB, _ := strconv.Atoi(envOr("HDFS_BLOCK_SIZE_MB", "16"))
	dir := envOr("HDFS_DIR", "/otus/files")
	if err := client.MkdirAll(dir, 0o755); err != nil && !os.IsExist(err) {
		_ = client.Close()
		return nil, err
	}
	slog.Info("хранилище HDFS подключено", "namenodes", addrs, "dir", dir,
		"replication", replication, "block_mb", blockMB)
	return &Store{
		client:      client,
		dir:         dir,
		replication: replication,
		blockSize:   int64(blockMB) << 20,
	}, nil
}

func (s *Store) full(name string) string {
	return path.Join(s.dir, path.Base(name))
}

// Put заливает файл потоком. Существующий файл перезаписывается: HDFS не
// умеет менять файл на месте, только создать заново.
func (s *Store) Put(_ context.Context, name string, r io.Reader) (File, error) {
	if s == nil {
		return File{}, ErrNotConfigured
	}
	start := time.Now()
	target := s.full(name)
	_ = s.client.Remove(target)
	w, err := s.client.CreateFile(target, s.replication, s.blockSize, 0o644)
	if err != nil {
		observability.ObserveStorage("hdfs", "put", start, err)
		return File{}, err
	}
	written, err := io.Copy(w, r)
	if err != nil {
		_ = w.Close()
		observability.ObserveStorage("hdfs", "put", start, err)
		return File{}, err
	}
	// Close дожимает последний блок и закрывает файл на namenode: без него
	// файл остается нулевого размера. Namenode может ответить, что блоки
	// еще реплицируются - тогда закрытие повторяется с нарастающей паузой,
	// как это делает и штатный java-клиент.
	if err := closeWithRetry(w); err != nil {
		observability.ObserveStorage("hdfs", "put", start, err)
		return File{}, err
	}
	observability.ObserveStorage("hdfs", "put", start, nil)
	return File{Name: path.Base(name), Path: target, Size: written, Modified: time.Now()}, nil
}

func closeWithRetry(w *hdfs.FileWriter) error {
	delay := 50 * time.Millisecond
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		if err = w.Close(); err == nil || !hdfs.IsErrReplicating(err) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
	return err
}

// Get открывает файл на чтение; offset/length задают частичное чтение.
func (s *Store) Get(_ context.Context, name string, offset, length int64) (io.ReadCloser, File, error) {
	if s == nil {
		return nil, File{}, ErrNotConfigured
	}
	start := time.Now()
	target := s.full(name)
	reader, err := s.client.Open(target)
	observability.ObserveStorage("hdfs", "get", start, err)
	if err != nil {
		return nil, File{}, err
	}
	info := reader.Stat()
	meta := File{Name: path.Base(name), Path: target, Size: info.Size(), Modified: info.ModTime()}
	if offset > 0 {
		if _, err := reader.Seek(offset, io.SeekStart); err != nil {
			_ = reader.Close()
			return nil, File{}, err
		}
	}
	if length > 0 {
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(reader, length), reader}, meta, nil
	}
	return reader, meta, nil
}

// List перечисляет файлы рабочего каталога.
func (s *Store) List(_ context.Context) ([]File, error) {
	if s == nil {
		return nil, ErrNotConfigured
	}
	start := time.Now()
	entries, err := s.client.ReadDir(s.dir)
	observability.ObserveStorage("hdfs", "list", start, err)
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files = append(files, File{
			Name:     e.Name(),
			Path:     path.Join(s.dir, e.Name()),
			Size:     e.Size(),
			Modified: e.ModTime(),
		})
	}
	return files, nil
}

// Stat возвращает метаданные файла.
func (s *Store) Stat(_ context.Context, name string) (File, error) {
	if s == nil {
		return File{}, ErrNotConfigured
	}
	start := time.Now()
	info, err := s.client.Stat(s.full(name))
	observability.ObserveStorage("hdfs", "stat", start, err)
	if err != nil {
		return File{}, err
	}
	return File{Name: info.Name(), Path: s.full(name), Size: info.Size(), Modified: info.ModTime()}, nil
}

// Dir возвращает рабочий каталог в HDFS.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
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
