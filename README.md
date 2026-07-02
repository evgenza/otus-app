# otus-app

HTTP-сервис на Go с хранением данных в PostgreSQL. Сделан для домашек по CI/CD и
тестированию микросервисов: собирается локально с линтером и тестами, в GitHub
Actions проверяется (юнит-тесты блокируют упаковку, интеграционные — отдельным
ручным треком), пакуется в Docker-образ, публикуется в Docker Hub и
автоматически разворачивается на сервере через docker compose. Для нагрузочного
тестирования есть сценарий на k6.

## Что умеет

HTTP-сервер на порту 8080:

- `GET /health` — `{"status":"работает"}`, нужен для health-check.
- `GET /version` — версия и дата сборки.
- `GET /hello` — `{"message":"Привет, мир!"}`, можно передать `?name=...`.
- `POST /messages` — принимает `{"text":"..."}`, сохраняет сообщение в БД,
  возвращает созданную запись с `id` и `created_at`.
- `GET /messages` — список последних сообщений из БД.

Версия и дата зашиваются в бинарь при компиляции через `-ldflags`
(пакет `internal/version`).

## Структура

```
cmd/app/main.go                       точка входа app: HTTP-сервер + graceful shutdown
cmd/gateway/main.go                   второй сервис: проксирует запросы в app
internal/handlers/                    роуты, обработчики и юнит-тесты
internal/storage/                     работа с PostgreSQL + интеграционные тесты
internal/observability/               логи (slog), метрики (Prometheus), трейсинг (OTel)
internal/version/                     версия сборки
loadtest/script.js                    нагрузочный сценарий k6
observability/                        стек наблюдаемости (Prometheus, Grafana, ELK, Jaeger)
.github/workflows/ci-cd.yml           основной пайплайн (lint, unit, build, deploy)
.github/workflows/integration.yml     интеграционные тесты (ручной запуск)
.github/workflows/loadtest.yml        нагрузочный тест k6 (ручной запуск)
Dockerfile                            multi-stage сборка образов app и gateway
docker-compose.yml                    запуск app + postgres
docs/REPORT.md                        отчёт по сборке/деплою
docs/TEST-REPORT.md                   протокол тестирования
docs/OBSERVABILITY.md                 протокол проверки наблюдаемости
```

Раскладка обычная для Go: то, что запускается, лежит в `cmd/`, внутренние
пакеты — во `internal/`. Хранилище подключается к обработчикам через интерфейс
`MessageStore`, поэтому юнит-тесты идут без реальной БД, а интеграционные — с ней.

## Требования

Go 1.26+, golangci-lint 2.x, Docker с плагином compose, make. Приложению нужна
переменная `DATABASE_URL` (строка подключения к PostgreSQL).

## Локально

Все команды — в Makefile, `make help` покажет список:

```bash
make fmt      # форматирование
make lint     # линтеры
make test     # юнит-тесты
make build    # бинарь в bin/otus-app
```

Прогнать всё разом перед коммитом:

```bash
make fmt lint test build
```

Проще всего поднять приложение вместе с базой через compose:

```bash
make docker-up          # app + postgres
curl localhost:8080/health
curl -X POST localhost:8080/messages -d '{"text":"привет"}'
curl localhost:8080/messages
make docker-down
```

Запуск бинаря напрямую (нужен поднятый Postgres):

```bash
export DATABASE_URL='postgres://otus:otus@localhost:5432/otus?sslmode=disable'
./bin/otus-app
```

## Тестирование

**Юнит-тесты** (без БД, через фейковое хранилище):

```bash
go test -race -cover ./...
```

**Интеграционные тесты** (API + реальный PostgreSQL, под build-тегом
`integration`). Нужен запущенный Postgres и `DATABASE_URL`:

```bash
docker run -d --name pg -e POSTGRES_USER=otus -e POSTGRES_PASSWORD=otus \
  -e POSTGRES_DB=otus -p 5432:5432 postgres:16-alpine
DATABASE_URL='postgres://otus:otus@localhost:5432/otus?sslmode=disable' \
  go test -tags=integration -race ./...
```

**Нагрузочное тестирование** (k6) против развёрнутого сервиса:

```bash
docker run --rm -e BASE_URL=http://82.202.142.225:8080 \
  -v "$PWD/loadtest:/loadtest" grafana/k6 run /loadtest/script.js
```

Протокол испытаний и анализ — в [docs/TEST-REPORT.md](docs/TEST-REPORT.md).

## Наблюдаемость

Оба сервиса инструментированы: структурные JSON-логи (`slog`), метрики
Prometheus (`/metrics`, включая бизнес-метрику `otus_messages_created_total`) и
трейсинг OpenTelemetry (запросы к БД — через `otelpgx`). Трейсинг включается
переменной `OTEL_EXPORTER_OTLP_ENDPOINT` (без неё — выключен, приложение
работает как обычно).

Локальный стек наблюдаемости поднимается отдельно:

```bash
cd observability
cp .env.example .env          # заполнить SMTP для писем-алертов
docker compose up -d
```

После старта доступно: Grafana `:3000`, Prometheus `:9090`, Alertmanager
`:9093`, Jaeger `:16686`, Kibana `:5601`.

- **Логи** → Filebeat → Elasticsearch → Kibana (data view `otus-logs*`);
- **Метрики** → Prometheus → дашборд Grafana (провижнится автоматически);
- **Алерты** (`ServiceDown`, `HighErrorRate`, `HighLatency`) → Alertmanager →
  почта;
- **Трейсы** цепочки gateway→app→БД → Jaeger.

Секреты SMTP (логин/пароль/получатель) в репозиторий не коммитятся: локально —
в `observability/alertmanager/alertmanager.yml` (в `.gitignore`), на сервере CI
рендерит конфиг из GitHub secrets `SMTP_USERNAME` / `SMTP_PASSWORD` / `SMTP_TO`.

Протокол проверки — в [docs/OBSERVABILITY.md](docs/OBSERVABILITY.md).

## CI/CD

Основной пайплайн `ci-cd.yml`:

1. **lint** — gofmt и golangci-lint.
2. **unit tests** — `go test -race -cover` (блокирующий этап перед упаковкой).
3. **build-and-push** — собирает Docker-образ и пушит в Docker Hub
   (только после успешных lint и unit-тестов).
4. **deploy** — заходит на сервер по SSH, `docker compose pull` и `up -d`.

Отдельные треки:

- `integration.yml` — интеграционные тесты с сервисом Postgres. Запускаются на
  pull request в `main` (ловят регрессии по БД до merge) и вручную
  (`workflow_dispatch`).
- `loadtest.yml` — нагрузочный тест k6 по указанному URL, вручную
  (`workflow_dispatch`).

Секреты репозитория: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`, `SSH_HOST`,
`SSH_USER`, `SSH_PORT`, `SSH_KEY`, а также `SMTP_USERNAME`, `SMTP_PASSWORD`,
`SMTP_TO` для писем-алертов на сервере.

## Деплой на сервер

На сервере нужны Docker и плагин compose. Пайплайн копирует каталог
`observability/`, рендерит `alertmanager.yml` из секретов, логинится в Docker Hub
и поднимает стек наблюдаемости (`docker-compose.server.yml`: app, gateway, БД,
Prometheus, Grafana, Alertmanager, Jaeger). После деплоя на сервере доступны
Grafana `:3000`, Prometheus `:9090`, Jaeger `:16686`.
