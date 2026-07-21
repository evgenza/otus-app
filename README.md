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
- `GET /status` — статусная страница на `html/template`: версия, аптайм,
  состояние БД и контрольных сумм.
- `GET /swagger/` — Swagger UI, спецификация OpenAPI вшита в бинарь.
- `POST /messages` — принимает `{"text":"..."}`, сохраняет сообщение в БД,
  возвращает созданную запись с `id`, `created_at` и контрольной суммой текста.
  Требует JWT из Keycloak (заголовок `Authorization: Bearer ...`).
- `GET /messages` — список последних сообщений из БД, у каждого — результат
  проверки контрольной суммы (`checksum_ok`).

gRPC-сервер на порту 9091 (контракт — `api/proto/messages.proto`): unary
`CreateMessage`, server streaming `ListMessages`, client streaming
`BatchCreate`, bidirectional `Chat`. Создание закрыто тем же JWT, что и HTTP
(токен в метаданных `authorization`). Gateway выводит gRPC наружу мостом:
`POST /gw/grpc/messages`, `GET /gw/grpc/messages` (NDJSON-стрим),
`POST /gw/grpc/messages/batch`, `POST /gw/grpc/chat`.

Между gateway и app стоит chaos-прокси (`cmd/chaosproxy`): считает трафик в
обе стороны метриками Prometheus и по командам на управляющий порт вносит
помехи — задержку, повтор запросов, обрывы, снижение скорости.

Версия и дата зашиваются в бинарь при компиляции через `-ldflags`
(пакет `internal/version`).

## Структура

```
cmd/app/main.go                       точка входа app: HTTP + gRPC + graceful shutdown
cmd/gateway/main.go                   второй сервис: HTTP-прокси и gRPC-мост в app
cmd/chaosproxy/main.go                прокси с метриками трафика и управляемыми помехами
api/proto/messages.proto              gRPC-контракт (4 вида взаимодействия)
internal/grpcapi/                     сгенерированный protoc-ом код
internal/grpcserver/                  gRPC-сервер, интерцепторы авторизации, бенчмарки
internal/handlers/                    роуты, обработчики, статусная страница и юнит-тесты
internal/handlers/apidocs/            спецификация OpenAPI и Swagger UI
internal/httpserver/                  запуск HTTP-сервера с graceful shutdown
internal/security/                    mTLS, проверка JWT, контрольные суммы
internal/storage/                     работа с PostgreSQL + интеграционные тесты
internal/observability/               логи (slog), метрики (Prometheus), трейсинг (OTel)
internal/version/                     версия сборки
loadtest/script.js                    нагрузочный сценарий k6
observability/                        стек наблюдаемости и безопасности (Prometheus, Grafana,
                                      ELK, Jaeger, Keycloak, nginx)
observability/certs/gen-certs.sh      генерация внутреннего CA и сертификатов mTLS
observability/keycloak/               шаблон realm Keycloak
observability/nginx/                  конфиг реверс-прокси с TLS
.github/workflows/ci-cd.yml           основной пайплайн (lint, unit, build, deploy)
.github/workflows/integration.yml     интеграционные тесты (ручной запуск)
.github/workflows/loadtest.yml        нагрузочный тест k6 (ручной запуск)
Dockerfile                            multi-stage сборка образов app и gateway
docker-compose.yml                    запуск app + postgres
docs/REPORT.md                        отчёт по сборке/деплою
docs/TEST-REPORT.md                   протокол тестирования
docs/OBSERVABILITY.md                 протокол проверки наблюдаемости
docs/SECURITY.md                      протокол проверки защиты (TLS, JWT, хеширование)
docs/INTERACTION.md                   протокол проверки взаимодействия (Swagger, gRPC, прокси)
```

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

Проще всего поднять всё через compose в `observability/` (перед первым запуском
сгенерировать сертификаты и отрендерить realm Keycloak):

```bash
cd observability
sh certs/gen-certs.sh
GRAFANA_OAUTH_SECRET=dev DEMO_USER_PASSWORD=demo123 \
  envsubst < keycloak/realm-otus.json.tmpl > keycloak/realm-otus.json
docker compose up -d
```

App принимает только mTLS-подключения, поэтому curl ходит с клиентским
сертификатом:

```bash
curl --cacert certs/ca.crt --cert certs/gateway.crt --key certs/gateway.key \
  https://localhost:8080/health
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

**Сравнение HTTP+JSON и gRPC** (бенчмарки одинаковых операций):

```bash
make bench
```

**Нагрузочное тестирование** (k6) против развёрнутого сервиса:

```bash
docker run --rm -e BASE_URL=https://zhemchugovei.duckdns.org \
  -e AUTH_USER=evgenza -e AUTH_PASS='пароль пользователя' \
  -v "$PWD/loadtest:/loadtest" grafana/k6 run /loadtest/script.js
```

Сценарий сам получает JWT в Keycloak (`setup()`) и шлёт `POST /messages` с
заголовком `Authorization`.

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
cp alertmanager/alertmanager.yml.tmpl alertmanager/alertmanager.yml   # вписать SMTP-креды
docker compose up -d
```

После старта доступно: Grafana `:3000`, Prometheus `:9090`, Alertmanager
`:9093`, Jaeger `:16686/jaeger`, Kibana `:5601`, Keycloak `:8081/auth`.

- **Логи** → Filebeat → Elasticsearch → Kibana (data view `otus-logs*`);
- **Метрики** → Prometheus → дашборд Grafana;
- **Алерты** (`ServiceDown`, `HighErrorRate`, `HighLatency`) → Alertmanager →
  почта;
- **Трейсы** цепочки gateway→app→БД → Jaeger.

Протокол проверки — в [docs/OBSERVABILITY.md](docs/OBSERVABILITY.md).

## Безопасность

- **mTLS между сервисами.** App принимает только TLS-подключения с клиентским
  сертификатом, подписанным внутренним CA (`observability/certs/gen-certs.sh`).
  Клиентские сертификаты есть у gateway, nginx и Prometheus. Включается
  переменными `TLS_CERT_FILE` / `TLS_KEY_FILE` / `TLS_CA_FILE` (без них сервис
  работает по HTTP — так гоняются юнит- и интеграционные тесты).
- **TLS снаружи.** nginx терминирует HTTPS с сертификатом Let's Encrypt для
  `zhemchugovei.duckdns.org`, порт 80 отдаёт только редирект и ACME-челлендж.
- **JWT через Keycloak.** `POST /messages` требует токен: app скачивает JWKS
  из Keycloak (`AUTH_JWKS_URL`), проверяет подпись RS256, срок действия и
  издателя (`AUTH_ISSUER`). Grafana логинится через тот же Keycloak (OAuth),
  а Prometheus, Alertmanager и Jaeger закрыты oauth2-proxy — nginx пускает к
  ним только с сессией Keycloak (`auth_request`).
- **Хеширование данных.** При сохранении сообщения считается SHA-256 текста и
  кладётся в БД рядом с ним; при чтении хеш пересчитывается — подмена данных в
  обход API видна по `checksum_ok: false`.

Получить токен и создать сообщение:

```bash
TOKEN=$(curl -s -X POST \
  https://zhemchugovei.duckdns.org/auth/realms/otus/protocol/openid-connect/token \
  -d grant_type=password -d client_id=otus-app \
  -d username=evgenza -d password='...' | jq -r .access_token)
curl -X POST https://zhemchugovei.duckdns.org/messages \
  -H "Authorization: Bearer $TOKEN" -d '{"text":"привет"}'
```

Протокол проверки — в [docs/SECURITY.md](docs/SECURITY.md).

## CI/CD

Основной пайплайн `ci-cd.yml`:

1. **lint** — gofmt и golangci-lint.
2. **unit tests** — `go test -race -cover` (блокирующий этап перед упаковкой).
3. **security** — gitleaks (поиск секретов в истории), govulncheck и trivy
   (уязвимости зависимостей), валидация конфигов nginx / Keycloak /
   oauth2-proxy / Alertmanager на тестовых значениях (`scripts/check-configs.sh`).
4. **build-and-push** — собирает Docker-образ и пушит в Docker Hub
   (только после успешных lint, unit-тестов и security).
5. **deploy** — заходит на сервер по SSH, `docker compose pull` и `up -d`.
6. **security smoke** — после деплоя негативные проверки на живом сервере
   (`scripts/security-smoke.sh`): интерфейсы наблюдаемости без сессии Keycloak
   закрыты, `POST /messages` без токена — 401, HTTP уводится на HTTPS.

Отдельные треки:

- `integration.yml` — интеграционные тесты с сервисом Postgres. Запускаются на
  pull request в `main` (ловят регрессии по БД до merge) и вручную
  (`workflow_dispatch`).
- `loadtest.yml` — нагрузочный тест k6 по указанному URL, вручную
  (`workflow_dispatch`).

Секреты репозитория: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`, `SSH_HOST`,
`SSH_USER`, `SSH_PORT`, `SSH_KEY`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_TO`
(письма-алерты), `KEYCLOAK_ADMIN_PASSWORD`, `GRAFANA_ADMIN_PASSWORD`,
`GRAFANA_OAUTH_SECRET`, `OAUTH2_PROXY_CLIENT_SECRET`, `OAUTH2_PROXY_COOKIE_SECRET`,
`DEMO_USER_PASSWORD` (Keycloak, Grafana и oauth2-proxy).

## Деплой на сервер

На сервере нужны Docker и плагин compose. Пайплайн рендерит из шаблонов конфиг
Alertmanager и realm Keycloak (секреты подставляются через `envsubst`), копирует
каталог `observability/` на сервер, генерирует внутренний CA и сертификаты mTLS,
при первом деплое выпускает сертификат Let's Encrypt, и поднимает стек
(`docker-compose.server.yml`: nginx, app, gateway, chaos-прокси, Keycloak, БД, Prometheus,
Grafana, Alertmanager, Jaeger, ELK, certbot). Чтобы всё влезло в 4Gi памяти,
тяжёлым контейнерам заданы `mem_limit` и ужаты heap-ы (Elasticsearch и
Keycloak — по 256m).

Наружу открыты только 80/443, всё ходит через nginx:

- <https://zhemchugovei.duckdns.org/> — API app (`/hello`, `/messages`, ...)
- <https://zhemchugovei.duckdns.org/swagger/> — Swagger UI
- <https://zhemchugovei.duckdns.org/status> — статусная страница
- <https://zhemchugovei.duckdns.org/gw/messages> — gateway (HTTP+JSON через
  chaos-прокси), <https://zhemchugovei.duckdns.org/gw/grpc/messages> — gRPC-мост
- <https://zhemchugovei.duckdns.org/auth/> — Keycloak
- <https://zhemchugovei.duckdns.org/grafana/> — Grafana (вход через Keycloak)
- <https://zhemchugovei.duckdns.org/prometheus/>, `/alertmanager/`, `/jaeger/`,
  `/kibana/` — за oauth2-proxy: без сессии Keycloak nginx отправляет на
  страницу логина
