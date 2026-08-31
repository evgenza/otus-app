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

.PHONY: brokers-up
brokers-up: ## Поднять стенд брокеров (Kafka, RabbitMQ, NATS)
	docker compose -f ds/docker-compose.brokers.yml build app
	docker compose -f ds/docker-compose.brokers.yml up -d

.PHONY: brokers-test
brokers-test: ## Тесты брокеров под нагрузкой: отказы узлов и масштабирование
	bash scripts/broker-failover-test.sh

.PHONY: brokers-down
brokers-down: ## Остановить стенд брокеров
	docker compose -f ds/docker-compose.brokers.yml down -v

.PHONY: storage-up
storage-up: ## Поднять стенд хранилищ (MinIO, HDFS, Cassandra, Elasticsearch)
	docker compose -f ds/docker-compose.storage.yml build app
	docker compose -f ds/docker-compose.storage.yml up -d

.PHONY: storage-test
storage-test: ## Тесты хранилищ под нагрузкой с отказами узлов
	bash scripts/storage-failover-test.sh

.PHONY: s3-test
s3-test: ## Теги, версии, частичная и multipart-загрузка в S3
	bash scripts/s3-features-test.sh

.PHONY: cassandra-test
cassandra-test: ## Запросы к Cassandra и уровни консистентности
	bash scripts/cassandra-queries-test.sh

.PHONY: search-bench
search-bench: ## Сравнение поиска: Elasticsearch, PostgreSQL, Cassandra
	bash scripts/search-benchmark.sh

.PHONY: storage-down
storage-down: ## Остановить стенд хранилищ
	docker compose -f ds/docker-compose.storage.yml down -v

.PHONY: bench
bench: ## Сравнение производительности HTTP+JSON и gRPC
	go test -run '^$$' -bench . -benchtime 2s ./internal/grpcserver/

.PHONY: proto
proto: ## Перегенерировать gRPC-код из api/proto (нужны protoc и плагины)
	protoc --proto_path=api/proto \
		--go_out=internal/grpcapi --go_opt=paths=source_relative \
		--go-grpc_out=internal/grpcapi --go-grpc_opt=paths=source_relative \
		messages.proto

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
