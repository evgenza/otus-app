package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evgenza/otus-app/internal/blobstore"
	"github.com/evgenza/otus-app/internal/broker"
	"github.com/evgenza/otus-app/internal/cstore"
	"github.com/evgenza/otus-app/internal/hdfsstore"
	"github.com/evgenza/otus-app/internal/search"
)

type fakeBlobs struct {
	data     map[string][]byte
	tags     map[string]map[string]string
	versions map[string][]blobstore.Object
	lastPut  int64
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{
		data:     make(map[string][]byte),
		tags:     make(map[string]map[string]string),
		versions: make(map[string][]blobstore.Object),
	}
}

func (f *fakeBlobs) Put(_ context.Context, key string, r io.Reader, size int64,
	_ string, tags map[string]string,
) (blobstore.Object, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return blobstore.Object{}, err
	}
	f.lastPut = size
	f.data[key] = body
	f.tags[key] = tags
	obj := blobstore.Object{
		Key: key, Size: int64(len(body)), VersionID: "v" + key, Tags: tags, IsLatest: true,
	}
	f.versions[key] = append([]blobstore.Object{obj}, f.versions[key]...)
	return obj, nil
}

func (f *fakeBlobs) Get(_ context.Context, key, _ string, offset, length int64) (io.ReadCloser, blobstore.Object, error) {
	body, ok := f.data[key]
	if !ok {
		return nil, blobstore.Object{}, errors.New("нет объекта")
	}
	if offset > int64(len(body)) {
		return nil, blobstore.Object{}, errors.New("смещение за концом объекта")
	}
	body = body[offset:]
	if length > 0 && length < int64(len(body)) {
		body = body[:length]
	}
	return io.NopCloser(bytes.NewReader(body)), blobstore.Object{
		Key: key, Size: int64(len(body)), VersionID: "v" + key,
	}, nil
}

func (f *fakeBlobs) List(_ context.Context, prefix string) ([]blobstore.Object, error) {
	objects := make([]blobstore.Object, 0, len(f.data))
	for key, body := range f.data {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, blobstore.Object{Key: key, Size: int64(len(body))})
		}
	}
	return objects, nil
}

func (f *fakeBlobs) Versions(_ context.Context, key string) ([]blobstore.Object, error) {
	return f.versions[key], nil
}

func (f *fakeBlobs) Tags(_ context.Context, key, _ string) (map[string]string, error) {
	return f.tags[key], nil
}

func (f *fakeBlobs) SetTags(_ context.Context, key string, tags map[string]string) error {
	f.tags[key] = tags
	return nil
}

func (f *fakeBlobs) Stat(_ context.Context, key, _ string) (blobstore.Object, error) {
	body, ok := f.data[key]
	if !ok {
		return blobstore.Object{}, errors.New("нет объекта")
	}
	return blobstore.Object{Key: key, Size: int64(len(body)), VersionID: "v" + key}, nil
}

func (f *fakeBlobs) FindByTag(ctx context.Context, key, value, prefix string) ([]blobstore.Object, error) {
	all, _ := f.List(ctx, prefix)
	found := make([]blobstore.Object, 0)
	for _, obj := range all {
		if v, ok := f.tags[obj.Key][key]; ok && (value == "" || v == value) {
			obj.Tags = f.tags[obj.Key]
			found = append(found, obj)
		}
	}
	return found, nil
}

func (f *fakeBlobs) Bucket() string { return "test-bucket" }

type fakeFiles struct{ data map[string][]byte }

func (f *fakeFiles) Put(_ context.Context, name string, r io.Reader) (hdfsstore.File, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return hdfsstore.File{}, err
	}
	if f.data == nil {
		f.data = make(map[string][]byte)
	}
	f.data[name] = body
	return hdfsstore.File{Name: name, Path: "/otus/files/" + name, Size: int64(len(body))}, nil
}

func (f *fakeFiles) Get(_ context.Context, name string, offset, length int64) (io.ReadCloser, hdfsstore.File, error) {
	body, ok := f.data[name]
	if !ok {
		return nil, hdfsstore.File{}, errors.New("нет файла")
	}
	body = body[offset:]
	if length > 0 && length < int64(len(body)) {
		body = body[:length]
	}
	return io.NopCloser(bytes.NewReader(body)), hdfsstore.File{Name: name, Size: int64(len(body))}, nil
}

func (f *fakeFiles) List(_ context.Context) ([]hdfsstore.File, error) {
	files := make([]hdfsstore.File, 0, len(f.data))
	for name, body := range f.data {
		files = append(files, hdfsstore.File{Name: name, Size: int64(len(body))})
	}
	return files, nil
}

func (f *fakeFiles) Stat(_ context.Context, name string) (hdfsstore.File, error) {
	body, ok := f.data[name]
	if !ok {
		return hdfsstore.File{}, errors.New("нет файла")
	}
	return hdfsstore.File{Name: name, Size: int64(len(body))}, nil
}

func (f *fakeFiles) Dir() string { return "/otus/files" }

type fakeEvents struct {
	items []cstore.Event
}

func (f *fakeEvents) Recent(_ context.Context, _ string, limit int) ([]cstore.Event, error) {
	if len(f.items) > limit {
		return f.items[:limit], nil
	}
	return f.items, nil
}

func (f *fakeEvents) ByID(_ context.Context, id int64) (cstore.Event, error) {
	for _, ev := range f.items {
		if ev.ID == id {
			return ev, nil
		}
	}
	return cstore.Event{}, errors.New("нет события")
}

func (f *fakeEvents) Scan(_ context.Context, substr string, limit int) ([]cstore.Event, int, error) {
	found := make([]cstore.Event, 0)
	for _, ev := range f.items {
		if len(found) < limit && strings.Contains(ev.Text, substr) {
			found = append(found, ev)
		}
	}
	return found, len(f.items), nil
}

type fakeSearch struct {
	docs    []search.Document
	files   []search.FileDoc
	indexed []search.FileDoc
}

func (f *fakeSearch) SearchMessages(_ context.Context, query string, limit int) (search.Result, error) {
	res := search.Result{TookMS: 1}
	for _, doc := range f.docs {
		if len(res.Messages) < limit && strings.Contains(doc.Text, query) {
			res.Messages = append(res.Messages, doc)
		}
	}
	res.Total = int64(len(res.Messages))
	return res, nil
}

func (f *fakeSearch) SearchFiles(_ context.Context, tagKey, tagValue, _ string, limit int) (search.Result, error) {
	res := search.Result{TookMS: 1}
	for _, doc := range f.files {
		if v, ok := doc.Tags[tagKey]; ok && (tagValue == "" || v == tagValue) && len(res.Files) < limit {
			res.Files = append(res.Files, doc)
		}
	}
	res.Total = int64(len(res.Files))
	return res, nil
}

func (f *fakeSearch) IndexFile(_ context.Context, doc search.FileDoc) error {
	f.indexed = append(f.indexed, doc)
	return nil
}

type fakeBus struct{ events []broker.Event }

func (f *fakeBus) Publish(_ context.Context, ev broker.Event) { f.events = append(f.events, ev) }
func (f *fakeBus) Names() []string                            { return []string{"kafka", "rabbitmq", "nats"} }

func request(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCreateMessagePublishesEvent(t *testing.T) {
	bus := &fakeBus{}
	handler := New(&fakeStore{}, nil, WithBus(bus))

	rec := request(t, handler, http.MethodPost, "/messages", `{"text":"событие в брокер"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ожидался статус 201, получен %d", rec.Code)
	}
	if len(bus.events) != 1 {
		t.Fatalf("ожидалось одно событие в шине, получено %d", len(bus.events))
	}
	if bus.events[0].Text != "событие в брокер" {
		t.Fatalf("в брокер ушел не тот текст: %q", bus.events[0].Text)
	}
}

func TestUploadAndDownloadObject(t *testing.T) {
	blobs := newFakeBlobs()
	index := &fakeSearch{}
	handler := New(&fakeStore{}, nil, WithBlobs(blobs), WithSearch(index))

	rec := request(t, handler, http.MethodPost, "/files?name=report.bin&tags=kind=report,owner=evgenza",
		"содержимое файла", map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("ожидался статус 201, получен %d: %s", rec.Code, rec.Body.String())
	}
	if string(blobs.data["report.bin"]) != "содержимое файла" {
		t.Fatalf("в хранилище легло не то содержимое: %q", blobs.data["report.bin"])
	}
	if blobs.tags["report.bin"]["kind"] != "report" {
		t.Fatalf("теги не проставлены: %v", blobs.tags["report.bin"])
	}
	if len(index.indexed) != 1 || index.indexed[0].Key != "report.bin" {
		t.Fatalf("метаданные файла не проиндексированы: %v", index.indexed)
	}

	rec = request(t, handler, http.MethodGet, "/files/report.bin", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "содержимое файла" {
		t.Fatalf("файл скачался неверно: %d %q", rec.Code, rec.Body.String())
	}
}

func TestDownloadRangeReturnsPartialContent(t *testing.T) {
	blobs := newFakeBlobs()
	handler := New(&fakeStore{}, nil, WithBlobs(blobs))
	if _, err := blobs.Put(context.Background(), "data.bin",
		strings.NewReader("0123456789"), 10, "application/octet-stream", nil); err != nil {
		t.Fatal(err)
	}

	rec := request(t, handler, http.MethodGet, "/files/data.bin", "", map[string]string{"Range": "bytes=2-5"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("ожидался статус 206, получен %d", rec.Code)
	}
	if rec.Body.String() != "2345" {
		t.Fatalf("вернулся не тот кусок: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("неверный Content-Range: %q", got)
	}
}

func TestUploadToHDFS(t *testing.T) {
	files := &fakeFiles{}
	handler := New(&fakeStore{}, nil, WithFiles(files))

	rec := request(t, handler, http.MethodPost, "/files?name=big.bin&backend=hdfs", "большой файл", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ожидался статус 201, получен %d: %s", rec.Code, rec.Body.String())
	}
	if string(files.data["big.bin"]) != "большой файл" {
		t.Fatalf("в HDFS легло не то содержимое: %q", files.data["big.bin"])
	}

	rec = request(t, handler, http.MethodGet, "/files/big.bin?backend=hdfs", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "большой файл" {
		t.Fatalf("файл из HDFS скачался неверно: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Backend") != "hdfs" {
		t.Fatalf("не проставлен бэкенд в заголовке: %q", rec.Header().Get("X-Backend"))
	}
}

func TestFilesVersionsAndTags(t *testing.T) {
	blobs := newFakeBlobs()
	handler := New(&fakeStore{}, nil, WithBlobs(blobs))

	for _, body := range []string{"первая версия", "вторая версия"} {
		if rec := request(t, handler, http.MethodPost, "/files?name=doc.txt", body, nil); rec.Code != http.StatusCreated {
			t.Fatalf("загрузка не удалась: %d", rec.Code)
		}
	}
	rec := request(t, handler, http.MethodGet, "/files/doc.txt/versions", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	var versions struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &versions); err != nil {
		t.Fatal(err)
	}
	if versions.Total != 2 {
		t.Fatalf("ожидались две версии, получено %d", versions.Total)
	}

	rec = request(t, handler, http.MethodPut, "/files/doc.txt/tags", `{"kind":"doc"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("не удалось обновить теги: %d", rec.Code)
	}
	if blobs.tags["doc.txt"]["kind"] != "doc" {
		t.Fatalf("теги не заменились: %v", blobs.tags["doc.txt"])
	}
}

func TestSearchFilesByTagUsesIndex(t *testing.T) {
	index := &fakeSearch{files: []search.FileDoc{
		{Key: "a.bin", Backend: "s3", Tags: map[string]string{"kind": "report"}},
		{Key: "b.bin", Backend: "s3", Tags: map[string]string{"kind": "log"}},
	}}
	handler := New(&fakeStore{}, nil, WithSearch(index), WithBlobs(newFakeBlobs()))

	rec := request(t, handler, http.MethodGet, "/files?tag=kind=report", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался статус 200, получен %d", rec.Code)
	}
	var res struct {
		Source string `json:"source"`
		Total  int64  `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Source != "elasticsearch" || res.Total != 1 {
		t.Fatalf("поиск по тегу ушел не туда: %+v", res)
	}
}

func TestSearchEngines(t *testing.T) {
	index := &fakeSearch{docs: []search.Document{{ID: 1, Text: "квартальный отчет"}}}
	events := &fakeEvents{items: []cstore.Event{
		{ID: 1, Text: "квартальный отчет", CreatedAt: time.Now()},
		{ID: 2, Text: "лог доставки", CreatedAt: time.Now()},
	}}
	handler := New(&fakeStore{}, nil, WithSearch(index), WithEvents(events))

	for _, tc := range []struct {
		engine string
		want   string
	}{
		{"elasticsearch", "elasticsearch"},
		{"cassandra", "cassandra"},
	} {
		rec := request(t, handler, http.MethodGet, "/search?q=отчет&engine="+tc.engine, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("движок %s ответил %d", tc.engine, rec.Code)
		}
		var res struct {
			Engine  string `json:"engine"`
			Total   int    `json:"total"`
			Scanned int    `json:"scanned"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Engine != tc.want || res.Total != 1 {
			t.Fatalf("движок %s вернул %+v", tc.engine, res)
		}
		if tc.engine == "cassandra" && res.Scanned != 2 {
			t.Fatalf("перебор в Cassandra должен пройти по всем строкам, прошел %d", res.Scanned)
		}
	}

	if rec := request(t, handler, http.MethodGet, "/search", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("пустой запрос должен давать 400, получен %d", rec.Code)
	}
	if rec := request(t, handler, http.MethodGet, "/search?q=x&engine=неизвестный", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестный движок должен давать 400, получен %d", rec.Code)
	}
}

func TestEventsEndpoints(t *testing.T) {
	events := &fakeEvents{items: []cstore.Event{{ID: 7, Text: "событие", CreatedAt: time.Now()}}}
	handler := New(&fakeStore{}, nil, WithEvents(events))

	if rec := request(t, handler, http.MethodGet, "/events", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("лента событий ответила %d", rec.Code)
	}
	if rec := request(t, handler, http.MethodGet, "/events/7", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("событие по id ответило %d", rec.Code)
	}
	if rec := request(t, handler, http.MethodGet, "/events/999", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("несуществующее событие должно давать 404, получен %d", rec.Code)
	}
}

func TestStorageEndpointsWithoutBackends(t *testing.T) {
	handler := New(&fakeStore{}, nil)
	for _, path := range []string{"/files", "/files/x", "/files/x/versions", "/events", "/search?q=x"} {
		rec := request(t, handler, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("без хранилища %s должен отвечать 503, получен %d", path, rec.Code)
		}
	}
}

func TestParseHelpers(t *testing.T) {
	tags := parseTags("kind=report, owner=evgenza ,мусор")
	if len(tags) != 2 || tags["kind"] != "report" || tags["owner"] != "evgenza" {
		t.Fatalf("теги разобраны неверно: %v", tags)
	}
	if parseTags("  ") != nil {
		t.Fatal("пустая строка тегов должна давать nil")
	}

	for _, tc := range []struct {
		header string
		offset int64
		length int64
		ok     bool
	}{
		{"bytes=0-99", 0, 100, true},
		{"bytes=100-", 100, 0, true},
		{"bytes=5-1", 5, 0, true},
		{"", 0, 0, false},
		{"items=1-2", 0, 0, false},
	} {
		offset, length, ok := parseRange(tc.header)
		if offset != tc.offset || length != tc.length || ok != tc.ok {
			t.Fatalf("Range %q разобран как (%d, %d, %v)", tc.header, offset, length, ok)
		}
	}

	if k, v := splitTag("kind=report"); k != "kind" || v != "report" {
		t.Fatalf("фильтр по тегу разобран неверно: %q %q", k, v)
	}
	if k, v := splitTag("kind"); k != "kind" || v != "" {
		t.Fatalf("фильтр без значения разобран неверно: %q %q", k, v)
	}
}
