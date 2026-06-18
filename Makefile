APP_NAME := otus-app
IMAGE    := evgenza/otus-app
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/evgenza/otus-app/internal/version
LDFLAGS := -s -w \
  -X $(PKG).Version=$(VERSION) \
  -X $(PKG).Date=$(DATE)

.DEFAULT_GOAL := help

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: fmt
fmt:
	gofmt -w .
	golangci-lint fmt ./...

.PHONY: lint
lint: ## линтеры
	golangci-lint run ./...

.PHONY: test
test: ## Юнит-тесты с детектором гонок и покрытием
	go test -race -cover ./...

DATABASE_URL ?= postgres://otus:otus@localhost:5432/otus?sslmode=disable
BASE_URL     ?= http://82.202.142.225:8080

.PHONY: test-integration
test-integration: ## Интеграционные тесты (нужен Postgres и DATABASE_URL)
	DATABASE_URL="$(DATABASE_URL)" go test -tags=integration -race -v ./...

.PHONY: loadtest
loadtest: ## Нагрузочный тест k6 (BASE_URL задаёт цель)
	docker run --rm -e BASE_URL="$(BASE_URL)" -v "$(PWD)/loadtest:/loadtest" grafana/k6 run /loadtest/script.js

.PHONY: build
build: ## Собрать бинарь
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/app

.PHONY: run
run: build ## Собрать и запустить
	./bin/$(APP_NAME)

.PHONY: docker-build
docker-build: ## Собрать Docker-образ
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: docker-up
docker-up: ## Поднять через docker compose
	docker compose up -d

.PHONY: docker-down
docker-down: ## Остановить compose-стек
	docker compose down

.PHONY: clean
clean: ## Удалить артефакты сборки
	rm -rf bin
