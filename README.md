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
| `internal/identity/` | Accounts, teams, tokens, sessions, grants |
| `internal/limit/` | Per-IP rate limiting and client-IP resolution |
| `migrations/` | Numbered SQL, applied by `migrate` |
| `web/` | React app (embedded into the binary at build time) |
| `deploy/compose/` | Dev stack (postgres + migrate + seed + api + worker) |
| `deploy/k8s/` | Reference k8s manifests (production lives in a separate deploy repo) |
| `docs/api.md` | The `/api/v1` contract |
| `design_handoff_varianthub/` | Design reference (prototype HTML + spec) |

## Quick start

```sh
make dev                              # build and bring the stack up
open http://localhost:18080           # the web app
```

The stack builds the React app into the Go binary, so one container serves both
the UI and the API. On first load the app asks you to create an administrator
account, using the **bootstrap token** the server printed to its log at startup:

```
docker compose -f deploy/compose/docker-compose.yml logs api | grep cgl_vhb_
```

That token stops working the moment the account exists. Everything after that is
an email and password, or a personal API token for scripts.

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
| `make build` | build the React app **and** the binary that embeds it |
| `make build-api` | Go-only binary, no web UI (needs no node) |
| `make ui-dev` | Vite dev server on :5173, proxying to the API |

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

## Provisioning source data

A registered source declares where its data comes from; it still has to be
downloaded before it can annotate. Until it is, annotating fails with
`sources not downloaded — run varhub download`.

Two ways to do it:

- **Sources & snapshots → Sources.** An unprovisioned source shows a storage
  dropdown and a Download button on its own row; a provisioned one shows its
  footprint instead. The gap and the fix are in the same place.
- **Storage & files** lists the configured locations and what each holds; **View**
  opens a tree of that location's contents.

Provisioning runs as a queued job — watch it under **System jobs**.

Provisioning is per **source**, not per snapshot — a source is the unit of data,
and a newly registered one has to be downloadable before anyone bundles it.

**Put the storage somewhere with room.** Reference data is large — GENCODE is
~130 MB, dbSNP ~24 GB — and the default is a docker volume on the root
filesystem. Point it at a real disk with `VHW_HOST_STORAGE` in
`deploy/compose/.env` (see `.env.example`); it is bind-mounted at `/mnt/storage`,
so nothing else has to change. The directory must be writable by the container
user (uid 10001) — `chmod 777` on a dev box, or chown it in production.

A source is annotated from wherever it was downloaded: the storage location *is*
the source cache. A job reads one location at a time, so sources split across two
locations is an error rather than a confusing "sources not downloaded".

**Storage locations** come from two places, deliberately:

- **Filesystem paths** are declared by the deployment in `VHW_STORAGE_PATHS`. A
  path only means anything if the worker has it mounted, so it is a deployment
  decision, not a runtime one. They are reconciled at startup — a path removed
  from the config stops being offered.
- **S3 buckets** are added through the admin UI, since they need no mount.

S3 targets can be configured but are **not yet selectable**: `varhub` cannot
download to a bucket yet. The UI shows them and says why rather than offering a
target that would fail at job time.

Files are not individually deletable. Each belongs to a source, and removing one
would break that source — delete the source instead.

## Adding sources and snapshots

There is no admin UI yet, so the catalog is managed from the command line. A
source is a **varhub source fragment** — exactly the TOML `varhub source add`
writes under `annotations/sources/`:

```sh
varianthub-web source add clinvar.toml          # derives name/version/kind
varianthub-web source add --private cosmic.toml
varianthub-web source list

varianthub-web snapshot add clinical-v4 \
  --build GRCh38 --title "GRCh38 Clinical v4" \
  --source clinvar-2026-06 --source gnomad-4.1 \
  --default clinvar_sig,gnomad_af --publish

varianthub-web snapshot list
```

The fragment text is stored verbatim and handed to `varhub` unchanged; the
columns beside it are a projection derived at write time for listing and
filtering. A snapshot stays a **draft** until `--publish`.

`snapshot add` reads the snapshot back after writing it, so a bad set of pins
fails there rather than at job time. Note that some source kinds have their own
requirements — a `genelist`, for instance, needs a `gtf = "name:version"` pointing
at a GTF source in the same snapshot, since flagging variants by gene membership
needs a gene model.

There is no `source remove` yet; detach a source from its snapshots and delete the
row directly if you need to.

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `VHW_ADDR` | `:8080` | Listen address (inside the container) |
| `VHW_DATABASE_URL` | — | Postgres DSN (required) |
| `VHW_ALLOW_ANONYMOUS` | `false` | Let callers with no account use the annotation flow |
| `VHW_CILOGON_CLIENT_ID` | — | CILogon OIDC client id (all three required to enable institutional sign-in) |
| `VHW_CILOGON_CLIENT_SECRET` | — | CILogon OIDC client secret |
| `VHW_CILOGON_REDIRECT_URL` | — | e.g. `https://varianthub.example/auth/cilogon/callback` |
| `VHW_CILOGON_AUTO_PROVISION_DOMAINS` | — | Email domains auto-provisioned on first sign-in; empty means invite-only |
| `VHW_WORKERS` | `2` | Worker pool size |
| `VHW_VARHUB_BIN` | `varhub` | Path to the CLI the worker execs |
| `VHW_VARHUB_HOME` | — | Fixed annotation config dir; empty = materialize per job from the catalog |
| `VHW_DATA_DIR` | `/var/lib/varianthub/data` | Shared, persistent: tool images and reference files |
| `VHW_STORAGE_PATHS` | `default=/mnt/storage` | Filesystem download targets, `name=/abs/path`, comma-separated; first is the default. Must be set on **both** `api` and `worker` — `serve` reconciles them into the catalog |
| `VHW_HOST_STORAGE` | *(named volume)* | Compose only: absolute host path to bind-mount at `/mnt/storage` |
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
