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
| `cmd/varianthub-web/` | One binary: `serve`, `worker`, `migrate`, `seed` |
| `internal/api/` | HTTP handlers and router (`/api/v1`) |
| `internal/queue/` | Postgres-backed job queue |
| `internal/catalog/` | Sources and snapshots in Postgres; materializes config per job |
| `internal/runner/` | The annotation seam — `Runner` interface + `ExecRunner` |
| `internal/auth/` | HMAC bearer tokens |
| `internal/limit/` | Per-IP rate limiting and client-IP resolution |
| `migrations/` | Numbered SQL, applied by `migrate` |
| `web/` | React app |
| `deploy/compose/` | Dev stack (postgres + migrate + seed + api + worker) |
| `deploy/k8s/` | Reference k8s manifests (production lives in a separate deploy repo) |
| `docs/api.md` | The `/api/v1` contract |
| `design_handoff_varianthub/` | Design reference (prototype HTML + spec) |

## Quick start

```sh
make dev                              # build and bring the stack up
curl localhost:18080/healthz          # {"status":"ok"}
```

That brings up Postgres, applies migrations, seeds a starter snapshot into the
catalog, and starts the API and a worker. Ordering is enforced by the compose
file, so nothing comes up against an unmigrated schema or an empty catalog:

```
postgres (healthy) ─► migrate ─► seed ─┬─► api
                                       └─► worker
```

| Command | |
|---|---|
| `make dev` | build + up |
| `make dev-logs` | follow logs |
| `make dev-down` | stop, **keep** data |
| `make dev-reset` | stop, **delete** volumes for a clean start |
| `make dev-psql` | psql shell on the dev database |

The image contains both `varianthub-web` and `varhub`; the CLI is compiled from
your local checkout, so the stack exercises the CLI you actually have. Point
`VARHUB_SRC` at it if it isn't a sibling directory:

```sh
VARHUB_SRC=/path/to/varianthub-cli make dev
```

Ports default to **18080** (API) and **55441** (Postgres) rather than 8080/5432,
since a dev machine usually already has something on those. Override with
`API_PORT` and `POSTGRES_PORT`.

Building outside Docker needs Go 1.25+ and a `varhub` on `PATH` for the worker.

## Deploying

`deploy/compose` is the supported path that ships with this repo: it works from a
clean checkout and is what self-hosters should use.

`deploy/k8s` holds reference manifests. Our own production deployment is managed
in a separate k8s deploy repo, so those files are examples and seed material, not
the live configuration — see that directory's README.

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `VHW_ADDR` | `:8080` | Listen address (inside the container) |
| `VHW_DATABASE_URL` | — | Postgres DSN (required) |
| `VHW_MASTER_KEY` | — | HMAC key signing API tokens |
| `VHW_REQUIRE_TOKEN` | `true` | Bearer auth on `/api/v1` |
| `VHW_WORKERS` | `2` | Worker pool size |
| `VHW_VARHUB_BIN` | `varhub` | Path to the CLI the worker execs |
| `VHW_VARHUB_HOME` | — | Fixed annotation config dir; empty = materialize per job from the catalog |
| `VHW_DATA_DIR` | `/var/lib/varianthub/data` | Shared, persistent: downloaded source files |
| `VHW_CACHE_DIR` | `/var/lib/varianthub/cache` | Shared, persistent: built indexes and cache |
| `VHW_JOB_TTL` | `24h` | Terminal jobs GC'd after this |
| `VHW_RATE_PER_MIN` | `30` | Per-IP submit rate |
| `VHW_MAX_JOBS_PER_IP` | `2` | Per-IP concurrent running jobs |

## How a job runs

1. A job row lands in Postgres (`queue`).
2. A worker claims it with `FOR UPDATE SKIP LOCKED`, so N workers take N distinct jobs.
3. The worker materializes that snapshot's config from the catalog into a temp
   directory — `config.toml` plus an `annotations/` tree — and execs
   `varhub annotate --format json` against it.
4. The result JSON is stored verbatim; the temp directory is removed.

Only the *config* is per-job. `VHW_DATA_DIR` and `VHW_CACHE_DIR` are shared and
persistent: they hold downloaded source files and the indexes built from them,
which would cost gigabytes to refetch per job.

## Status

Early. The queue, catalog, runner seam, and ops endpoints work; the `/api/v1`
surface in `docs/api.md` is being built out. See the design handoff for the
target product.
