COMPOSE = docker compose -f docker-compose.local.yml
TEST_COMPOSE = docker compose -p jungle-tests -f docker-compose.test.yml
export DATABASE_URL ?= postgres://jungle:jungle@localhost:5432/jungle?sslmode=disable
export TEST_DATABASE_URL ?= postgres://jungle_test:jungle_test@localhost:55432/jungle_test?sslmode=disable

.PHONY: run build test race vet fmt check up down migrate-up migrate-down migrate-status test-db-up test-db-down test-integration

run:
	go run ./cmd/server

build:
	go build -o bin/server ./cmd/server
	go build -o bin/migrate ./cmd/migrate

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal migrations tests

check: test race vet

up:
	$(COMPOSE) up --build

down:
	$(COMPOSE) down

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

migrate-status:
	go run ./cmd/migrate status

test-db-up:
	$(TEST_COMPOSE) up -d --wait

test-db-down:
	$(TEST_COMPOSE) down

test-integration:
	go test -race -tags=integration ./tests/integration/...
