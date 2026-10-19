COMPOSE = docker compose -f docker-compose.local.yml
TEST_COMPOSE = docker compose -p jungle-tests -f docker-compose.test.yml
export DATABASE_URL ?= postgres://jungle:jungle@localhost:5432/jungle?sslmode=disable
export TEST_DATABASE_URL ?= postgres://jungle_test:jungle_test@localhost:55432/jungle_test?sslmode=disable
export TEST_OIDC_ISSUER_URL ?= http://localhost:58081/realms/wagercore
export TEST_SQS_ENDPOINT ?= http://localhost:54566

.PHONY: run build test race vet fmt check up down migrate-up migrate-down migrate-status test-db-up test-db-down test-deps-up test-deps-down test-integration test-concurrency test-failover benchmark

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

test-deps-up: test-db-up

test-deps-down: test-db-down

test-integration:
	./scripts/test-integration.sh

test-concurrency:
	TEST_TIMEOUT=3m ./scripts/test-integration.sh -run 'Test(ThreeProcesses|SQSInbox)'

test-failover:
	TEST_TIMEOUT=3m ./scripts/test-integration.sh -run 'Test(Crash|ReferenceWorker|MessagingRetention|AtomicRollback|PostgresOutage|SQSOutage|Permanent|Shutdown)'

benchmark:
	WAGER_LOAD_TEST=1 TEST_TIMEOUT=10m ./scripts/test-integration.sh -run '^TestFinancialLoad$$' -v
