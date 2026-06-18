# Протокол тестирования otus-app

## Объём тестирования

Сервис `otus-app` — HTTP API на Go с хранением сообщений в PostgreSQL.
Проверяли три уровня:

1. **Юнит-тесты** — логика обработчиков без БД (через фейковое хранилище).
2. **Интеграционные тесты** — связка API + реальный PostgreSQL.
3. **Нагрузочное тестирование** — поведение под нагрузкой (один эндпоинт и
   E2E-цепочка `POST /messages → GET /messages`, то есть приложение → БД).

## 1. Юнит-тесты

Пакет `internal/handlers`. Хранилище подключается через интерфейс
`MessageStore`, в тестах используется фейковая реализация — БД не нужна.

Покрытие: `/health`, `/version`, `/hello` (с именем и без), `POST /messages`
(успех, пустой `text` → 400), `GET /messages` (список, ошибка хранилища → 500).

```bash
$ go test -race -cover ./...
ok   github.com/evgenza/otus-app/internal/handlers   coverage: ~95% of statements
```

В CI это этап **unit tests** — он блокирует сборку образа: `build-and-push`
объявлен с `needs: [lint, test]`, поэтому при падении юнит-тестов упаковка не
запускается.

## 2. Интеграционные тесты

Пакет `internal/storage`, файл под build-тегом `integration` (в обычный
`go test ./...` не попадает). Поднимают реальный Postgres и проверяют:

- `TestStorageCreateAndList` — запись и чтение через слой хранилища;
- `TestAPIWithDatabase` — сквозной сценарий: HTTP `POST /messages`, затем
  `GET /messages` против httptest-сервера с реальной БД.

Запуск локально:

```bash
$ DATABASE_URL='postgres://otus:otus@localhost:5432/otus?sslmode=disable' \
    go test -tags=integration -race -v ./internal/storage/
=== RUN   TestStorageCreateAndList
--- PASS: TestStorageCreateAndList (0.02s)
=== RUN   TestAPIWithDatabase
--- PASS: TestAPIWithDatabase (0.01s)
PASS
```

В CI — отдельный **ручной** трек `integration.yml` (`workflow_dispatch`) с
сервисным контейнером Postgres. Запускается отдельно от основного пайплайна.

![Интеграционные тесты в CI](screenshots/05-integration.png)

## 3. Нагрузочное тестирование

Инструмент — **k6**, сценарий `loadtest/script.js`. Два параллельных сценария:

| Сценарий | Что нагружает | Профиль (ramping-vus) |
|----------|---------------|------------------------|
| `single` | `GET /hello` (один сервис) | 0→50 VU за 30с, 50 VU 1 мин, спад 15с |
| `e2e`    | `POST /messages` + `GET /messages` (app → БД) | 0→20 VU за 30с, 20 VU 1 мин, спад 15с |

Пороги приёмки (thresholds):

- доля ошибок `http_req_failed` < 1%;
- `single`: `p(95)` времени ответа < 200 мс;
- `e2e`: `p(95)` времени ответа < 500 мс.

Запуск:

```bash
docker run --rm -e BASE_URL=http://82.202.142.225:8080 \
  -v "$PWD/loadtest:/loadtest" grafana/k6 run /loadtest/script.js
```

### Результаты

Прогон ~1 мин 45 с, суммарно до 70 одновременных VU.

| Метрика | Значение |
|---------|----------|
| Всего HTTP-запросов | 56 706 (≈539 req/s) |
| Итераций сценариев | 48 655 (≈463 it/s) |
| Доля ошибок (`http_req_failed`) | 0.00% |
| Проверок (checks) пройдено | 113 412 / 113 412 (100%) |
| `http_req_duration` p95 (`single`) | 0.65 мс |
| `http_req_duration` p95 (`e2e`) | 3.36 мс |
| `http_req_duration` max | 60.1 мс |

Все пороги пройдены, ошибок нет.

![Результаты k6](screenshots/06-k6.png)

### Оценка результатов

- Сервис держит нагрузку без ошибок: 0% неуспешных запросов на ~57 тыс. запросов.
- Чтение (`/hello`) очень дешёвое: p95 < 1 мс — узких мест на уровне приложения нет.
- E2E-цепочка с записью в БД ожидаемо дороже (p95 3.36 мс), но с большим запасом
  до порога 500 мс — связка app → PostgreSQL работает стабильно.
- Редкие выбросы до ~60 мс — это разогрев пула соединений и GC в начале прогона,
  на перцентили влияния почти нет.

Вывод: при выбранном профиле нагрузки сервис ведёт себя предсказуемо и проходит
все критерии приёмки. Возможные следующие шаги — поднять целевые VU/RPS до
появления деградации, чтобы найти предел пропускной способности.

## Pull request

[PR с тестированием приложения](https://github.com/evgenza/otus-app/pulls)
