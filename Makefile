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
TLS_PORT     ?= 18443
TEST_DB_URL  ?= postgres://postgres:test@localhost:$(TEST_DB_PORT)/varianthub?sslmode=disable

# The React app is part of the build: `make build` produces a binary that serves
# both the API and the web UI. Node is therefore a build dependency — use
# `build-api` for a Go-only binary (CI checks, API-only deployments).
UI_DIST      := web/embed/dist/index.html
UI_DEPS      := web/node_modules/.package-lock.json
UI_SOURCES   := $(shell find web/src web/public -type f 2>/dev/null) \
                web/index.html web/vite.config.ts web/package.json

.DEFAULT_GOAL := build
.PHONY: build build-api test test-unit test-integration vet fmt tidy clean \
        run-api run-worker migrate dev dev-down dev-reset dev-logs dev-psql \
        image test-db test-db-stop ui ui-dev help

## build: build the web UI and the binary that embeds it (the normal build)
build: $(UI_DIST)
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

## build-api: build the binary only, without the web UI (Go-only; API is complete)
build-api:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

## ui: build the React app into web/embed/dist
ui: $(UI_DIST)

# Rebuilds only when a UI source actually changed, so `make build` stays fast.
$(UI_DIST): $(UI_DEPS) $(UI_SOURCES)
	npm --prefix web run build

# npm ci only when the lockfile moves. Its own output file is the stamp, so this
# does not re-run on every build.
$(UI_DEPS): web/package-lock.json
	npm --prefix web ci
	@touch $@

## ui-dev: run the Vite dev server (proxies /api to a local API on 18080)
ui-dev: $(UI_DEPS)
	npm --prefix web run dev

## test: run every test (integration tests skip unless their env vars are set)
test:
	go test -race ./...

## test-unit: run only tests that need no external services
test-unit:
	go test -race ./internal/limit/... ./internal/api/...

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

## dev: build and bring up postgres + migrate + seed + api + worker
dev:
	$(COMPOSE) up --build -d
	@echo
	@echo "api:      http://localhost:$${API_PORT:-18080}/healthz"
	@echo "postgres: localhost:$${POSTGRES_PORT:-55441}"

## dev-tls: bring the stack up behind Caddy on https, API not exposed directly
# VH_SITE names the certificate; the default is this machine's hostname with an
# internal CA. API_BIND keeps the plain-http port on loopback so https is the
# only way in from the network.
dev-tls:
	@VH_HOST=$${VH_HOST:-$$(hostname)} API_BIND=127.0.0.1 \
	  $(COMPOSE) --profile tls up --build -d
	@echo
	@echo "web: https://$$(hostname):$(TLS_PORT)"
	@echo
	@echo "The certificate comes from Caddy's internal CA, so a browser warns"
	@echo "until that CA is trusted. Export it with:"
	@echo "  make dev-tls-ca > caddy-root.crt"

## dev-tls-ca: print Caddy's internal CA certificate, to trust in a browser or OS
dev-tls-ca:
	@$(COMPOSE) exec -T caddy cat /data/caddy/pki/authorities/local/root.crt

## dev-down: stop the stack (keeps volumes, so data survives)
dev-down:
	$(COMPOSE) --profile tls down

## dev-reset: stop the stack and DELETE its volumes (database + annotation config)
dev-reset:
	$(COMPOSE) down -v

## dev-logs: follow compose logs
dev-logs:
	$(COMPOSE) logs -f

## dev-psql: open a psql shell against the dev database
dev-psql:
	$(COMPOSE) exec postgres psql -U varianthub -d varianthub

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

## clean: remove build artifacts (keeps node_modules)
clean:
	rm -rf bin dist web/embed/dist/assets web/embed/dist/index.html \
	       web/embed/dist/favicon.svg

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
