package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/evgenza/otus-app/internal/blobstore"
	"github.com/evgenza/otus-app/internal/cstore"
	"github.com/evgenza/otus-app/internal/hdfsstore"
	"github.com/evgenza/otus-app/internal/search"
)

// Blobs — работа с объектным хранилищем S3.
type Blobs interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string, tags map[string]string) (blobstore.Object, error)
	Get(ctx context.Context, key, version string, offset, length int64) (io.ReadCloser, blobstore.Object, error)
	List(ctx context.Context, prefix string) ([]blobstore.Object, error)
	Versions(ctx context.Context, key string) ([]blobstore.Object, error)
	Tags(ctx context.Context, key, version string) (map[string]string, error)
	SetTags(ctx context.Context, key string, tags map[string]string) error
	Stat(ctx context.Context, key, version string) (blobstore.Object, error)
	FindByTag(ctx context.Context, key, value, prefix string) ([]blobstore.Object, error)
	Bucket() string
}

// Files — работа с файловым хранилищем HDFS.
type Files interface {
	Put(ctx context.Context, name string, r io.Reader) (hdfsstore.File, error)
	Get(ctx context.Context, name string, offset, length int64) (io.ReadCloser, hdfsstore.File, error)
	List(ctx context.Context) ([]hdfsstore.File, error)
	Stat(ctx context.Context, name string) (hdfsstore.File, error)
	Dir() string
}

// Events — лента событий в Cassandra.
type Events interface {
	Recent(ctx context.Context, day string, limit int) ([]cstore.Event, error)
	ByID(ctx context.Context, id int64) (cstore.Event, error)
	Scan(ctx context.Context, substr string, limit int) ([]cstore.Event, int, error)
}

// Search — поиск в Elasticsearch.
type Search interface {
	SearchMessages(ctx context.Context, query string, limit int) (search.Result, error)
	SearchFiles(ctx context.Context, tagKey, tagValue, backend string, limit int) (search.Result, error)
	IndexFile(ctx context.Context, doc search.FileDoc) error
}

// MessageSearcher — поиск по основному хранилищу (PostgreSQL), нужен для
// сравнения с полнотекстовым поиском.
type MessageSearcher interface {
	Search(ctx context.Context, query string, limit int) ([]Message, error)
}

// WithBlobs подключает S3.
func WithBlobs(b Blobs) Option { return func(a *API) { a.blobs = b } }

// WithFiles подключает HDFS.
func WithFiles(f Files) Option { return func(a *API) { a.files = f } }

// WithEvents подключает ленту событий Cassandra.
func WithEvents(e Events) Option { return func(a *API) { a.events = e } }

// WithSearch подключает Elasticsearch.
func WithSearch(s Search) Option { return func(a *API) { a.search = s } }

const maxUploadSize = 2 << 30 // 2 ГиБ на файл

// searchMessages сравнивает три способа найти текст: полнотекстовый индекс
// Elasticsearch, перебор таблицы в Cassandra и LIKE в PostgreSQL.
func (a *API) searchMessages(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "параметр q обязателен"})
		return
	}
	limit := intParam(r, "limit", 10, 1000)
	engine := r.URL.Query().Get("engine")
	if engine == "" {
		engine = "elasticsearch"
	}

	start := time.Now()
	switch engine {
	case "elasticsearch", "es":
		if a.search == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "поиск не настроен"})
			return
		}
		res, err := a.search.SearchMessages(r.Context(), query, limit)
		if err != nil {
			slog.ErrorContext(r.Context(), "поиск не удался", "engine", engine, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "поиск не удался"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"engine":     "elasticsearch",
			"query":      query,
			"total":      res.Total,
			"took_ms":    res.TookMS,
			"elapsed_ms": time.Since(start).Milliseconds(),
			"messages":   res.Messages,
		})
	case "cassandra":
		if a.events == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "лента событий не настроена"})
			return
		}
		found, scanned, err := a.events.Scan(r.Context(), query, limit)
		if err != nil {
			slog.ErrorContext(r.Context(), "поиск не удался", "engine", engine, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "поиск не удался"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"engine":     "cassandra",
			"query":      query,
			"total":      len(found),
			"scanned":    scanned,
			"elapsed_ms": time.Since(start).Milliseconds(),
			"events":     found,
		})
	case "postgres":
		searcher, ok := a.store.(MessageSearcher)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "поиск по БД не поддерживается"})
			return
		}
		msgs, err := searcher.Search(r.Context(), query, limit)
		if err != nil {
			slog.ErrorContext(r.Context(), "поиск не удался", "engine", engine, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "поиск не удался"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"engine":     "postgres",
			"query":      query,
			"total":      len(msgs),
			"elapsed_ms": time.Since(start).Milliseconds(),
			"messages":   msgs,
		})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "неизвестный движок поиска"})
	}
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	if a.events == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "лента событий не настроена"})
		return
	}
	events, err := a.events.Recent(r.Context(), r.URL.Query().Get("day"), intParam(r, "limit", 20, 1000))
	if err != nil {
		slog.ErrorContext(r.Context(), "не удалось прочитать ленту событий", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось прочитать ленту событий"})
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (a *API) getEvent(w http.ResponseWriter, r *http.Request) {
	if a.events == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "лента событий не настроена"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректный идентификатор"})
		return
	}
	ev, err := a.events.ByID(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "событие не найдено"})
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// uploadFile принимает тело запроса потоком и кладет его в S3 или HDFS.
// Размер заранее не известен при chunked-передаче: тогда S3-клиент сам
// режет поток на части и грузит его multipart-загрузкой.
func (a *API) uploadFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := path.Base(strings.TrimSpace(q.Get("name")))
	if name == "" || name == "." || name == "/" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "параметр name обязателен"})
		return
	}
	backend := backendParam(r)
	tags := parseTags(q.Get("tags"))
	body := http.MaxBytesReader(w, r.Body, maxUploadSize)
	defer func() { _ = body.Close() }()

	switch backend {
	case "hdfs":
		if a.files == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "HDFS не настроен"})
			return
		}
		file, err := a.files.Put(r.Context(), name, body)
		if err != nil {
			slog.ErrorContext(r.Context(), "не удалось записать файл в HDFS", "name", name, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "не удалось записать файл"})
			return
		}
		a.indexFile(r.Context(), search.FileDoc{
			Key: file.Name, Backend: "hdfs", Size: file.Size, Tags: tags, UpdatedAt: time.Now().UTC(),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"backend": "hdfs", "file": file, "tags": tags})
	default:
		if a.blobs == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "S3 не настроен"})
			return
		}
		contentType := r.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		obj, err := a.blobs.Put(r.Context(), name, body, contentLength(r), contentType, tags)
		if err != nil {
			slog.ErrorContext(r.Context(), "не удалось записать объект в S3", "key", name, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "не удалось записать объект"})
			return
		}
		a.indexFile(r.Context(), search.FileDoc{
			Key: obj.Key, Backend: "s3", Size: obj.Size, VersionID: obj.VersionID,
			Tags: tags, UpdatedAt: time.Now().UTC(),
		})
		writeJSON(w, http.StatusCreated, map[string]any{"backend": "s3", "object": obj})
	}
}

// downloadFile отдает файл целиком или кусок по заголовку Range: диапазон
// проксируется в хранилище, а не вырезается из полностью скачанного файла.
func (a *API) downloadFile(w http.ResponseWriter, r *http.Request) {
	key := path.Base(r.PathValue("key"))
	backend := backendParam(r)
	offset, length, hasRange := parseRange(r.Header.Get("Range"))

	var (
		reader io.ReadCloser
		size   int64
		total  int64
		err    error
	)
	switch backend {
	case "hdfs":
		if a.files == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "HDFS не настроен"})
			return
		}
		var meta hdfsstore.File
		if stat, statErr := a.files.Stat(r.Context(), key); statErr == nil {
			total = stat.Size
		}
		reader, meta, err = a.files.Get(r.Context(), key, offset, length)
		if err == nil {
			size = meta.Size
		}
	default:
		if a.blobs == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "S3 не настроен"})
			return
		}
		version := r.URL.Query().Get("version")
		if stat, statErr := a.blobs.Stat(r.Context(), key, version); statErr == nil {
			total = stat.Size
		}
		var obj blobstore.Object
		reader, obj, err = a.blobs.Get(r.Context(), key, version, offset, length)
		if err == nil {
			size = obj.Size
			if obj.VersionID != "" {
				w.Header().Set("X-Version-Id", obj.VersionID)
			}
		}
	}
	if err != nil {
		slog.WarnContext(r.Context(), "не удалось прочитать файл", "key", key, "backend", backend, "err", err)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "файл не найден"})
		return
	}
	defer func() { _ = reader.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Backend", backend)
	status := http.StatusOK
	if hasRange {
		if total == 0 {
			total = offset + size
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+size-1, total))
		w.Header().Set("Accept-Ranges", "bytes")
		status = http.StatusPartialContent
	}
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, reader); err != nil {
		slog.WarnContext(r.Context(), "передача файла прервана", "key", key, "err", err)
	}
}

// listFiles показывает разницу между поиском по тегам средствами S3 и
// поиском по индексу: source=s3 перебирает бакет, source=es спрашивает
// Elasticsearch.
func (a *API) listFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	backend := backendParam(r)
	tagKey, tagValue := splitTag(q.Get("tag"))
	source := q.Get("source")
	if source == "" {
		if tagKey != "" && a.search != nil {
			source = "es"
		} else {
			source = backend
		}
	}
	start := time.Now()

	switch source {
	case "es":
		if a.search == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "поиск не настроен"})
			return
		}
		res, err := a.search.SearchFiles(r.Context(), tagKey, tagValue, backend, intParam(r, "limit", 50, 1000))
		if err != nil {
			slog.ErrorContext(r.Context(), "не удалось найти файлы", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось найти файлы"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"source": "elasticsearch", "total": res.Total, "took_ms": res.TookMS,
			"elapsed_ms": time.Since(start).Milliseconds(), "files": res.Files,
		})
	case "hdfs":
		if a.files == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "HDFS не настроен"})
			return
		}
		files, err := a.files.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "не удалось получить список файлов"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"source": "hdfs", "total": len(files),
			"elapsed_ms": time.Since(start).Milliseconds(), "files": files,
		})
	default:
		if a.blobs == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "S3 не настроен"})
			return
		}
		var (
			objects []blobstore.Object
			err     error
		)
		if tagKey != "" {
			objects, err = a.blobs.FindByTag(r.Context(), tagKey, tagValue, q.Get("prefix"))
		} else {
			objects, err = a.blobs.List(r.Context(), q.Get("prefix"))
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "не удалось получить список объектов", "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "не удалось получить список объектов"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"source": "s3", "bucket": a.blobs.Bucket(), "total": len(objects),
			"elapsed_ms": time.Since(start).Milliseconds(), "files": objects,
		})
	}
}

func (a *API) listVersions(w http.ResponseWriter, r *http.Request) {
	if a.blobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "S3 не настроен"})
		return
	}
	key := path.Base(r.PathValue("key"))
	versions, err := a.blobs.Versions(r.Context(), key)
	if err != nil {
		slog.ErrorContext(r.Context(), "не удалось получить версии объекта", "key", key, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "не удалось получить версии объекта"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "total": len(versions), "versions": versions})
}

func (a *API) updateTags(w http.ResponseWriter, r *http.Request) {
	if a.blobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "S3 не настроен"})
		return
	}
	key := path.Base(r.PathValue("key"))
	var tags map[string]string
	if err := json.NewDecoder(r.Body).Decode(&tags); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректное тело запроса"})
		return
	}
	if err := a.blobs.SetTags(r.Context(), key, tags); err != nil {
		slog.ErrorContext(r.Context(), "не удалось обновить теги", "key", key, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "не удалось обновить теги"})
		return
	}
	obj, err := a.blobs.Stat(r.Context(), key, "")
	if err == nil {
		a.indexFile(r.Context(), search.FileDoc{
			Key: key, Backend: "s3", Size: obj.Size, VersionID: obj.VersionID,
			Tags: tags, UpdatedAt: time.Now().UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "tags": tags})
}

// indexFile дублирует метаданные файла в Elasticsearch: индекс дает поиск
// по тегам, которого нет в самом S3.
func (a *API) indexFile(ctx context.Context, doc search.FileDoc) {
	if a.search == nil {
		return
	}
	if err := a.search.IndexFile(ctx, doc); err != nil {
		slog.WarnContext(ctx, "не удалось проиндексировать файл", "key", doc.Key, "err", err)
	}
}

func backendParam(r *http.Request) string {
	if b := r.URL.Query().Get("backend"); b == "hdfs" {
		return "hdfs"
	}
	return "s3"
}

func contentLength(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return -1 // размер неизвестен: клиент S3 разложит поток на части сам
}

func intParam(r *http.Request, name string, fallback, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

// parseTags разбирает "k=v,k2=v2" в набор тегов.
func parseTags(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	tags := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if ok && k != "" {
			tags[k] = v
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// splitTag разбирает фильтр "k=v" или просто "k".
func splitTag(raw string) (key, value string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	k, v, _ := strings.Cut(raw, "=")
	return strings.TrimSpace(k), strings.TrimSpace(v)
}

// parseRange понимает "bytes=start-end" и "bytes=start-".
func parseRange(header string) (offset, length int64, ok bool) {
	raw, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found {
		return 0, 0, false
	}
	startRaw, endRaw, _ := strings.Cut(raw, "-")
	start, err := strconv.ParseInt(strings.TrimSpace(startRaw), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if strings.TrimSpace(endRaw) == "" {
		return start, 0, true
	}
	end, err := strconv.ParseInt(strings.TrimSpace(endRaw), 10, 64)
	if err != nil || end < start {
		return start, 0, true
	}
	return start, end - start + 1, true
}
