# varianthub-web build management.
#
# Pure-Go (pgx) ⇒ CGO_ENABLED=0 cross-compiles cleanly.

BIN     := varianthub-web
PKG     := ./cmd/varianthub-web
VERSION := $(shell v=$$(git describe --tags --always 2>/dev/null || echo dev); if ! git diff --quiet HEAD 2>/dev/null; then v=$$(echo "$$v" | sed 's/-g/-dev-g/'); echo "$$v" | grep -q -- -dev || v="$$v-dev"; fi; echo "$$v")
LDFLAGS := -s -w -X main.version=$(VERSION)
GO      := CGO_ENABLED=0 go

COMPOSE := docker compose -f deploy/compose/docker-compose.yml

# Local Postgres used by `make test-db` and the queue tests.
TEST_DB_PORT ?= 55440
TEST_DB_URL  ?= postgres://postgres:test@localhost:$(TEST_DB_PORT)/varianthub?sslmode=disable

.DEFAULT_GOAL := build
.PHONY: build test test-unit test-integration vet fmt tidy clean run-api run-worker \
        migrate dev dev-down dev-logs image test-db test-db-stop help

## build: compile for the host platform into bin/
build:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

## test: run every test (integration tests skip unless their env vars are set)
test:
	go test -race ./...

## test-unit: run only tests that need no external services
test-unit:
	go test -race ./internal/auth/... ./internal/limit/... ./internal/api/...

## test-integration: run the full suite against a local Postgres + varhub
# VHW_TEST_VARHUB defaults to a varhub built from a sibling varianthub-cli checkout.
test-integration: test-db
	VHW_TEST_DATABASE_URL='$(TEST_DB_URL)' \
	VHW_TEST_VARHUB=$${VHW_TEST_VARHUB:-../varianthub-cli/bin/varhub} \
	go test -race -count=1 ./...

## test-db: start the throwaway Postgres the integration tests use
test-db:
	@docker inspect vhw-pg-test >/dev/null 2>&1 || \
	  docker run -d --name vhw-pg-test \
	    -e POSTGRES_PASSWORD=test -e POSTGRES_DB=varianthub \
	    -p $(TEST_DB_PORT):5432 postgres:16-alpine >/dev/null
	@docker start vhw-pg-test >/dev/null 2>&1 || true
	@for i in $$(seq 1 30); do \
	  docker exec vhw-pg-test pg_isready -U postgres -q 2>/dev/null && exit 0; \
	  sleep 1; \
	done; echo "postgres did not become ready" >&2; exit 1

## test-db-stop: remove the throwaway Postgres
test-db-stop:
	@docker rm -f vhw-pg-test >/dev/null 2>&1 || true

## vet: run go vet
vet:
	go vet ./...

## fmt: format all Go files
fmt:
	gofmt -w .

## tidy: tidy go.mod
tidy:
	go mod tidy

## dev: bring up postgres + api + worker via docker compose
dev:
	$(COMPOSE) up --build -d
	@echo "api: http://localhost:8080/healthz"

## dev-down: tear down the compose stack (keeps the volume)
dev-down:
	$(COMPOSE) down

## dev-logs: follow compose logs
dev-logs:
	$(COMPOSE) logs -f

## migrate: apply pending migrations against VHW_DATABASE_URL
migrate: build
	./bin/$(BIN) migrate

## run-api: run the API server locally
run-api: build
	./bin/$(BIN) serve

## run-worker: run the job worker locally
run-worker: build
	./bin/$(BIN) worker

## image: build the container image
image:
	docker build -f deploy/compose/Dockerfile -t varianthub-web:$(VERSION) .

## clean: remove build artifacts
clean:
	rm -rf bin dist

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
