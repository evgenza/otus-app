// Package search индексирует сообщения и метаданные файлов в
// Elasticsearch и ищет по ним: полнотекстовый поиск с русской
// морфологией и поиск файлов по тегам.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"

	"github.com/evgenza/otus-app/internal/observability"
)

// ErrNotConfigured возвращается, когда Elasticsearch не настроен окружением.
var ErrNotConfigured = errors.New("elasticsearch не настроен")

// Document — проиндексированное сообщение.
type Document struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Checksum  string    `json:"checksum"`
	Producer  string    `json:"producer,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// FileDoc — метаданные файла из S3 или HDFS.
type FileDoc struct {
	Key       string            `json:"key"`
	Backend   string            `json:"backend"`
	Size      int64             `json:"size"`
	VersionID string            `json:"version_id,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Result — ответ поиска: найденные документы и время поиска на стороне ES.
type Result struct {
	Total    int64         `json:"total"`
	TookMS   int64         `json:"took_ms"`
	Messages []Document    `json:"messages,omitempty"`
	Files    []FileDoc     `json:"files,omitempty"`
	Elapsed  time.Duration `json:"-"`
}

type Index struct {
	client     *elasticsearch.Client
	msgIndex   string
	fileIndex  string
	refreshAll bool
}

// Русский анализатор: стоп-слова и стеммер, чтобы "сообщения" находилось
// по запросу "сообщение". Одна реплика на индекс — при трех узлах ES это
// дает копию каждого шарда на соседе.
const messagesMapping = `{
  "settings": {
    "number_of_shards": 3,
    "number_of_replicas": 1,
    "analysis": {
      "filter": {
        "ru_stop":    {"type": "stop", "stopwords": "_russian_"},
        "ru_stemmer": {"type": "stemmer", "language": "russian"}
      },
      "analyzer": {
        "ru": {"type": "custom", "tokenizer": "standard",
               "filter": ["lowercase", "ru_stop", "ru_stemmer"]}
      }
    }
  },
  "mappings": {
    "properties": {
      "id":         {"type": "long"},
      "text":       {"type": "text", "analyzer": "ru"},
      "checksum":   {"type": "keyword"},
      "producer":   {"type": "keyword"},
      "created_at": {"type": "date"}
    }
  }
}`

const filesMapping = `{
  "settings": {"number_of_shards": 1, "number_of_replicas": 1},
  "mappings": {
    "properties": {
      "key":        {"type": "keyword"},
      "backend":    {"type": "keyword"},
      "size":       {"type": "long"},
      "version_id": {"type": "keyword"},
      "tags":       {"type": "flattened"},
      "updated_at": {"type": "date"}
    }
  }
}`

// New подключается к Elasticsearch и создает индексы. Пустой ELASTIC_URLS
// означает, что интеграция выключена.
func New(ctx context.Context) (*Index, error) {
	addrs := envList("ELASTIC_URLS")
	if len(addrs) == 0 {
		return nil, nil
	}
	client, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses:     addrs,
		RetryOnStatus: []int{502, 503, 504, 429},
		MaxRetries:    3,
	})
	if err != nil {
		return nil, err
	}
	idx := &Index{
		client:    client,
		msgIndex:  envOr("ELASTIC_INDEX", "otus-messages"),
		fileIndex: envOr("ELASTIC_FILE_INDEX", "otus-files"),
		// refresh на каждую запись заставляет Elasticsearch немедленно
		// открывать новый сегмент - это дорого и на нагрузке съедает
		// сотни миллисекунд на запрос. По умолчанию выключено: документ
		// становится виден в пределах index.refresh_interval (1 секунда).
		refreshAll: envOr("ELASTIC_REFRESH", "false") == "true",
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := idx.ensure(initCtx, idx.msgIndex, messagesMapping); err != nil {
		return nil, err
	}
	if err := idx.ensure(initCtx, idx.fileIndex, filesMapping); err != nil {
		return nil, err
	}
	slog.Info("Elasticsearch подключен", "urls", addrs, "index", idx.msgIndex)
	return idx, nil
}

func (i *Index) ensure(ctx context.Context, name, mapping string) error {
	res, err := i.client.Indices.Exists([]string{name}, i.client.Indices.Exists.WithContext(ctx))
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode == 200 {
		return nil
	}
	create, err := i.client.Indices.Create(name,
		i.client.Indices.Create.WithBody(strings.NewReader(mapping)),
		i.client.Indices.Create.WithContext(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = create.Body.Close() }()
	if create.IsError() {
		body := readBody(create.Body)
		// Индекс мог создать другой экземпляр приложения между Exists и
		// Create - это не ошибка, а нормальная гонка на старте.
		if strings.Contains(body, "resource_already_exists_exception") {
			return nil
		}
		return fmt.Errorf("не удалось создать индекс %s: %s", name, body)
	}
	return nil
}

// IndexMessage кладет сообщение в индекс под его же идентификатором:
// повторная доставка события из брокера перезапишет документ, а не создаст
// дубль (идемпотентность на стороне потребителя).
func (i *Index) IndexMessage(ctx context.Context, doc Document) error {
	if i == nil {
		return ErrNotConfigured
	}
	start := time.Now()
	err := i.index(ctx, i.msgIndex, strconv.FormatInt(doc.ID, 10), doc)
	observability.ObserveStorage("elasticsearch", "index_message", start, err)
	return err
}

// IndexFile кладет в индекс метаданные файла вместе с тегами.
func (i *Index) IndexFile(ctx context.Context, doc FileDoc) error {
	if i == nil {
		return ErrNotConfigured
	}
	start := time.Now()
	err := i.index(ctx, i.fileIndex, doc.Backend+":"+doc.Key, doc)
	observability.ObserveStorage("elasticsearch", "index_file", start, err)
	return err
}

func (i *Index) index(ctx context.Context, index, id string, doc any) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	res, err := i.client.Index(index, bytes.NewReader(body),
		i.client.Index.WithDocumentID(id),
		i.client.Index.WithContext(ctx),
		i.client.Index.WithRefresh(refreshValue(i.refreshAll)))
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.IsError() {
		return errors.New(readBody(res.Body))
	}
	return nil
}

func refreshValue(on bool) string {
	if on {
		return "true"
	}
	return "false"
}

// SearchMessages ищет сообщения полнотекстовым запросом.
func (i *Index) SearchMessages(ctx context.Context, query string, limit int) (Result, error) {
	if i == nil {
		return Result{}, ErrNotConfigured
	}
	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"match": map[string]any{"text": map[string]any{"query": query, "operator": "and"}},
		},
		"sort": []any{map[string]any{"created_at": "desc"}},
	}
	raw, err := i.search(ctx, i.msgIndex, body)
	if err != nil {
		return Result{}, err
	}
	result := Result{Total: raw.Hits.Total.Value, TookMS: raw.Took, Elapsed: raw.elapsed}
	for _, hit := range raw.Hits.Hits {
		var doc Document
		if err := json.Unmarshal(hit.Source, &doc); err == nil {
			result.Messages = append(result.Messages, doc)
		}
	}
	return result, nil
}

// SearchFiles ищет файлы по тегу (и, опционально, по бэкенду). Это то, чего
// нет в самом S3: там теги можно только прочитать у известного объекта.
func (i *Index) SearchFiles(ctx context.Context, tagKey, tagValue, backend string, limit int) (Result, error) {
	if i == nil {
		return Result{}, ErrNotConfigured
	}
	filters := make([]any, 0, 2)
	if tagKey != "" {
		field := "tags." + tagKey
		if tagValue == "" {
			filters = append(filters, map[string]any{"exists": map[string]any{"field": field}})
		} else {
			filters = append(filters, map[string]any{"term": map[string]any{field: tagValue}})
		}
	}
	if backend != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"backend": backend}})
	}
	body := map[string]any{
		"size":  limit,
		"query": map[string]any{"bool": map[string]any{"filter": filters}},
	}
	raw, err := i.search(ctx, i.fileIndex, body)
	if err != nil {
		return Result{}, err
	}
	result := Result{Total: raw.Hits.Total.Value, TookMS: raw.Took, Elapsed: raw.elapsed}
	for _, hit := range raw.Hits.Hits {
		var doc FileDoc
		if err := json.Unmarshal(hit.Source, &doc); err == nil {
			result.Files = append(result.Files, doc)
		}
	}
	return result, nil
}

type searchResponse struct {
	Took int64 `json:"took"`
	Hits struct {
		Total struct {
			Value int64 `json:"value"`
		} `json:"total"`
		Hits []struct {
			Source json.RawMessage `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
	elapsed time.Duration
}

func (i *Index) search(ctx context.Context, index string, body map[string]any) (searchResponse, error) {
	start := time.Now()
	payload, err := json.Marshal(body)
	if err != nil {
		return searchResponse{}, err
	}
	res, err := i.client.Search(
		i.client.Search.WithIndex(index),
		i.client.Search.WithBody(bytes.NewReader(payload)),
		i.client.Search.WithTrackTotalHits(true),
		i.client.Search.WithContext(ctx))
	observability.ObserveStorage("elasticsearch", "search", start, err)
	if err != nil {
		return searchResponse{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.IsError() {
		return searchResponse{}, errors.New(readBody(res.Body))
	}
	var parsed searchResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return searchResponse{}, err
	}
	parsed.elapsed = time.Since(start)
	return parsed, nil
}

// Health возвращает статус кластера (green/yellow/red).
func (i *Index) Health(ctx context.Context) (string, error) {
	if i == nil {
		return "", ErrNotConfigured
	}
	res, err := i.client.Cluster.Health(i.client.Cluster.Health.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	var parsed struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return "", err
	}
	return parsed.Status, nil
}

func readBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 4096))
	return strings.TrimSpace(string(data))
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
