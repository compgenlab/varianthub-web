# VariantHub Web

The web front-end and REST API for [VariantHub](https://github.com/compgenlab/varianthub-cli) —
a React app plus a Go API server, backed by Postgres.

Annotation itself is **not** implemented here. This service queues jobs and hands each one
to the `varhub` CLI, which owns the annotation engine. That keeps the boundary at a process
edge rather than coupling two codebases to a shared 6,000-line library.

```
React (SPA) ──/api/v1──► Go API server ──► Postgres (jobs, catalog, auth)
                              │
                          worker ──exec──► varhub annotate --format json ──► S3
```

## Layout

| Path | What |
|---|---|
| `cmd/varianthub-web/` | One binary, three subcommands: `serve`, `worker`, `migrate` |
| `internal/api/` | HTTP handlers and router (`/api/v1`) |
| `internal/queue/` | Postgres-backed job queue |
| `internal/runner/` | The annotation seam — `Runner` interface + `ExecRunner` |
| `internal/auth/` | HMAC bearer tokens |
| `internal/limit/` | Per-IP rate limiting and client-IP resolution |
| `migrations/` | Numbered SQL, applied by `migrate` |
| `web/` | React app |
| `deploy/compose/`, `deploy/k8s/` | Deployment configs |
| `docs/api.md` | The `/api/v1` contract |
| `design_handoff_varianthub/` | Design reference (prototype HTML + spec) |

## Quick start

```sh
make dev          # docker compose: postgres + api
make migrate      # apply migrations
curl localhost:8080/healthz
```

Requires Go 1.25+ and a `varhub` binary on `PATH` for the worker.

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `VHW_ADDR` | `:8080` | Listen address |
| `VHW_DATABASE_URL` | — | Postgres DSN (required) |
| `VHW_MASTER_KEY` | — | HMAC key signing API tokens |
| `VHW_REQUIRE_TOKEN` | `true` | Bearer auth on `/api/v1` |
| `VHW_WORKERS` | `2` | Worker pool size |
| `VHW_VARHUB_BIN` | `varhub` | Path to the CLI the worker execs |
| `VHW_JOB_TTL` | `24h` | Terminal jobs GC'd after this |
| `VHW_RATE_PER_MIN` | `30` | Per-IP submit rate |
| `VHW_MAX_JOBS_PER_IP` | `2` | Per-IP concurrent running jobs |

## Status

Early. The queue, runner seam, and ops endpoints work; the `/api/v1` surface described in
`docs/api.md` is being built out. See the design handoff for the target product.
