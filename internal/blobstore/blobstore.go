package blobstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"

	"github.com/evgenza/otus-app/internal/observability"
)

// ErrNotConfigured возвращается, когда S3 не настроен переменными окружения.
var ErrNotConfigured = errors.New("S3 не настроен")

// Object — метаданные объекта в том виде, в котором их отдает API.
type Object struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag,omitempty"`
	VersionID    string            `json:"version_id,omitempty"`
	ContentType  string            `json:"content_type,omitempty"`
	LastModified time.Time         `json:"last_modified,omitzero"`
	Tags         map[string]string `json:"tags,omitempty"`
	IsLatest     bool              `json:"is_latest,omitempty"`
	Parts        int               `json:"parts,omitempty"`
}

type Store struct {
	client   *minio.Client
	bucket   string
	partSize uint64
}

// New подключается к S3 и готовит бакет: создает его, если нет, и включает
// версионирование. Пустой S3_ENDPOINT означает, что интеграция выключена.
func New(ctx context.Context) (*Store, error) {
	endpoint := strings.TrimSpace(os.Getenv("S3_ENDPOINT"))
	if endpoint == "" {
		return nil, nil
	}
	bucket := envOr("S3_BUCKET", "otus-files")
	useSSL := os.Getenv("S3_USE_SSL") == "true"
	partSize, err := strconv.ParseUint(envOr("S3_PART_SIZE_MB", "16"), 10, 64)
	if err != nil || partSize < 5 {
		partSize = 16
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(
			envOr("S3_ACCESS_KEY", "minioadmin"),
			envOr("S3_SECRET_KEY", "minioadmin"), ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	exists, err := client.BucketExists(initCtx, bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := client.MakeBucket(initCtx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, err
		}
	}
	// Версионирование нужно для задания со звездочкой: перезапись объекта
	// сохраняет предыдущую версию, а не затирает ее.
	if err := client.EnableVersioning(initCtx, bucket); err != nil {
		slog.Warn("не удалось включить версионирование бакета", "bucket", bucket, "err", err)
	}
	slog.Info("хранилище S3 подключено", "endpoint", endpoint, "bucket", bucket,
		"part_size_mb", partSize)
	return &Store{client: client, bucket: bucket, partSize: partSize << 20}, nil
}

// Put заливает объект. Размер -1 допустим: клиент сам разложит поток на
// части по S3_PART_SIZE_MB и отправит их multipart-загрузкой.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64,
	contentType string, tagset map[string]string,
) (Object, error) {
	if s == nil {
		return Object{}, ErrNotConfigured
	}
	start := time.Now()
	info, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType:           contentType,
		PartSize:              s.partSize,
		NumThreads:            4,
		ConcurrentStreamParts: size < 0,
		UserTags:              tagset,
	})
	observability.ObserveStorage("s3", "put", start, err)
	if err != nil {
		return Object{}, err
	}
	return Object{
		Key:         info.Key,
		Size:        info.Size,
		ETag:        info.ETag,
		VersionID:   info.VersionID,
		ContentType: contentType,
		Tags:        tagset,
		Parts:       partsFromETag(info.ETag),
	}, nil
}

// Get отдает объект целиком или его часть. offset/length задают диапазон
// байт (length <= 0 — до конца файла), version — конкретную версию.
func (s *Store) Get(ctx context.Context, key, version string, offset, length int64) (io.ReadCloser, Object, error) {
	if s == nil {
		return nil, Object{}, ErrNotConfigured
	}
	start := time.Now()
	opts := minio.GetObjectOptions{VersionID: version}
	if offset > 0 || length > 0 {
		if err := opts.SetRange(offset, rangeEnd(offset, length)); err != nil {
			return nil, Object{}, err
		}
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		observability.ObserveStorage("s3", "get", start, err)
		return nil, Object{}, err
	}
	// GetObject ленивый: реальный запрос уходит на первом Stat/Read.
	stat, err := obj.Stat()
	observability.ObserveStorage("s3", "get", start, err)
	if err != nil {
		_ = obj.Close()
		return nil, Object{}, err
	}
	return obj, Object{
		Key:          stat.Key,
		Size:         stat.Size,
		ETag:         stat.ETag,
		VersionID:    stat.VersionID,
		ContentType:  stat.ContentType,
		LastModified: stat.LastModified,
	}, nil
}

// List перечисляет объекты бакета по префиксу.
func (s *Store) List(ctx context.Context, prefix string) ([]Object, error) {
	if s == nil {
		return nil, ErrNotConfigured
	}
	start := time.Now()
	objects := make([]Object, 0)
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: prefix, Recursive: true,
	}) {
		if info.Err != nil {
			observability.ObserveStorage("s3", "list", start, info.Err)
			return nil, info.Err
		}
		objects = append(objects, Object{
			Key:          info.Key,
			Size:         info.Size,
			ETag:         info.ETag,
			VersionID:    info.VersionID,
			LastModified: info.LastModified,
		})
	}
	observability.ObserveStorage("s3", "list", start, nil)
	return objects, nil
}

// Versions возвращает все версии объекта, свежая — первой.
func (s *Store) Versions(ctx context.Context, key string) ([]Object, error) {
	if s == nil {
		return nil, ErrNotConfigured
	}
	start := time.Now()
	versions := make([]Object, 0)
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: key, Recursive: true, WithVersions: true,
	}) {
		if info.Err != nil {
			observability.ObserveStorage("s3", "versions", start, info.Err)
			return nil, info.Err
		}
		if info.Key != key {
			continue
		}
		versions = append(versions, Object{
			Key:          info.Key,
			Size:         info.Size,
			ETag:         info.ETag,
			VersionID:    info.VersionID,
			LastModified: info.LastModified,
			IsLatest:     info.IsLatest,
		})
	}
	observability.ObserveStorage("s3", "versions", start, nil)
	return versions, nil
}

// Tags читает теги объекта.
func (s *Store) Tags(ctx context.Context, key, version string) (map[string]string, error) {
	if s == nil {
		return nil, ErrNotConfigured
	}
	start := time.Now()
	t, err := s.client.GetObjectTagging(ctx, s.bucket, key, minio.GetObjectTaggingOptions{VersionID: version})
	observability.ObserveStorage("s3", "get_tags", start, err)
	if err != nil {
		return nil, err
	}
	return t.ToMap(), nil
}

// SetTags заменяет теги объекта целиком.
func (s *Store) SetTags(ctx context.Context, key string, tagset map[string]string) error {
	if s == nil {
		return ErrNotConfigured
	}
	start := time.Now()
	t, err := tags.NewTags(tagset, true)
	if err != nil {
		return err
	}
	err = s.client.PutObjectTagging(ctx, s.bucket, key, t, minio.PutObjectTaggingOptions{})
	observability.ObserveStorage("s3", "set_tags", start, err)
	return err
}

// FindByTag ищет объекты по тегу. В S3 нет поиска по тегам: приходится
// перебирать бакет и запрашивать теги каждого объекта — отдельным запросом
// на объект. Именно поэтому метаданные файлов дублируются в Elasticsearch.
func (s *Store) FindByTag(ctx context.Context, key, value, prefix string) ([]Object, error) {
	if s == nil {
		return nil, ErrNotConfigured
	}
	start := time.Now()
	all, err := s.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	found := make([]Object, 0)
	for _, obj := range all {
		t, err := s.Tags(ctx, obj.Key, "")
		if err != nil {
			continue
		}
		if v, ok := t[key]; ok && (value == "" || v == value) {
			obj.Tags = t
			found = append(found, obj)
		}
	}
	observability.ObserveStorage("s3", "find_by_tag", start, nil)
	return found, nil
}

// Stat возвращает метаданные объекта без чтения тела.
func (s *Store) Stat(ctx context.Context, key, version string) (Object, error) {
	if s == nil {
		return Object{}, ErrNotConfigured
	}
	start := time.Now()
	stat, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{VersionID: version})
	observability.ObserveStorage("s3", "stat", start, err)
	if err != nil {
		return Object{}, err
	}
	return Object{
		Key:          stat.Key,
		Size:         stat.Size,
		ETag:         stat.ETag,
		VersionID:    stat.VersionID,
		ContentType:  stat.ContentType,
		LastModified: stat.LastModified,
		Parts:        partsFromETag(stat.ETag),
	}, nil
}

// Bucket возвращает имя рабочего бакета.
func (s *Store) Bucket() string {
	if s == nil {
		return ""
	}
	return s.bucket
}

func rangeEnd(offset, length int64) int64 {
	if length <= 0 {
		return 0
	}
	return offset + length - 1
}

// partsFromETag: у multipart-объекта ETag имеет вид "<hash>-<число частей>".
func partsFromETag(etag string) int {
	if i := strings.LastIndex(etag, "-"); i > 0 {
		if n, err := strconv.Atoi(strings.Trim(etag[i+1:], `"`)); err == nil {
			return n
		}
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
