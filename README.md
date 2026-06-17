# otus-app

Небольшой HTTP-сервис на Go. Сделан для домашки по CI/CD: собирается локально с
линтером и тестами, в GitHub Actions проверяется, пакуется в Docker-образ,
публикуется в Docker Hub и автоматически разворачивается на сервере через
docker compose.

## Что умеет

Поднимает HTTP-сервер на порту 8080 с тремя ручками:

- `GET /health` — отвечает `{"status":"работает"}`, нужен для health-check.
- `GET /version` — версия и дата сборки.
- `GET /hello` — `{"message":"Привет, мир!"}`, можно передать `?name=...`.

Версия и дата зашиваются в бинарь при компиляции через `-ldflags`
(пакет `internal/version`).

## Структура

```
cmd/app/main.go            точка входа: HTTP-сервер и graceful shutdown
internal/handlers/         роуты и их тесты
internal/version/          версия сборки
.github/workflows/         пайплайн CI/CD
Dockerfile                 multi-stage сборка образа
docker-compose.yml         запуск на сервере
Makefile                   команды на каждый день
docs/REPORT.md             отчёт по ДЗ со скриншотами
```

Раскладка обычная для Go: то, что запускается, лежит в `cmd/`, остальное — во
`internal/` (наружу из модуля не импортируется).

## Локально

Все команды — в Makefile,
`make help` покажет список.

```bash
make fmt      # форматирование
make lint     # линтеры
make test     # тесты
make build    # бинарь в bin/otus-app
make run      # собрать и запустить
```

Прогнать всё разом перед коммитом:

```bash
make fmt lint test build
```

Запустить и проверить:

```bash
./bin/otus-app &
curl localhost:8080/health
curl 'localhost:8080/hello?name=otus'
```

## В Docker

```bash
make docker-build      # собрать образ evgenza/otus-app
make docker-up         # поднять через compose
make docker-down       # остановить
```

Образ собирается в два этапа: на golang:alpine компилируется статический бинарь,
в финальный alpine-образ кладётся только он. Контейнер запускается под обычным
пользователем и имеет HEALTHCHECK.

## CI/CD

Пайплайн в `.github/workflows/ci-cd.yml`, четыре этапа:

1. **lint** — проверка gofmt и golangci-lint.
2. **test** — `go test -race -cover` и контрольная сборка.
3. **build-and-push** — собирает Docker-образ и пушит в Docker Hub.
4. **deploy** — заходит на сервер по SSH, делает `docker compose pull` и
   `up -d`.

lint и test гоняются и на pull request. Сборка образа и деплой — только при
пуше в `main`.

## Деплой на сервер

На сервере нужны Docker и плагин compose. Пайплайн сам логинится в Docker Hub,
тянет свежий образ и перезапускает контейнер. Руками то же самое:

```bash
cd ~/otus-app
docker compose pull
docker compose up -d
```
