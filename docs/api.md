# VariantHub REST API (`/api/v1`)

The contract the React app is built against. Derived from the design handoff's
"Data the frontend needs from the backend" (`design_handoff_varianthub/README.md`).

**Implementation status.** The catalog, annotation, job, results and export
endpoints are **implemented**. Still outstanding: the admin surface, the
genome-browser tracks endpoint, job stage/percent, and HGVS/rsID input. Each
section is marked, and where the implementation deviates from the shape sketched
here it says so inline rather than leaving the doc aspirational.

This document is the agreed shape front-end work is built against; treat a change
here as a change to a shared contract.

---

## Conventions

- Base path `/api/v1` on the app's own host. The full base URL is per-deployment
  and must be read from config, never hardcoded in the client.
- All request and response bodies are JSON (`Content-Type: application/json`),
  except VCF upload, which is `multipart/form-data`.
- Timestamps are **Unix seconds** (integers), not RFC 3339. The client renders
  with `new Date(sec * 1000)`.
- Errors are `{"error": "<human-readable message>"}` with a meaningful status.
  Error text is safe to display; it never contains internal paths or stack traces.
- Genomic coordinates are 1-based, matching VCF.
- A variant may be written **either** dash-delimited (`chr17-7676154-C-T`) or
  colon-delimited (`chr17:7676154:C:T`). The engine parses the colon form, and the
  API rewrites the dash form to it. Translation is narrow by design: a token is
  only rewritten when it has no colon, exactly four non-empty dash-separated
  fields, and a numeric position — so colon-bearing HGVS and rsIDs pass through
  untouched.

### Authentication

`Authorization: Bearer <token>`.

*Superseded — see **Authentication** at the end of this document.* Chunk 1 issued
a single HMAC token signed with `VHW_MASTER_KEY`; that key is gone. Chunk 6 replaced
this with per-user tokens: `cgl_vh_` prefix + random secret, shown once at
creation, stored only as a hash, and carrying the owner's private-source grants.
A token must never grant more than its owner has.

Setting `VHW_REQUIRE_TOKEN=false` opens `/api/v1` entirely — intended for a
trusted network. The service refuses to start with auth on and an empty master
key, since HMAC over an empty key is forgeable by anyone.

### Status codes

| Code | Meaning |
|---|---|
| 200 | OK |
| 202 | Job accepted, not finished |
| 400 | Malformed request (bad JSON, unparseable variant, unknown annotation) |
| 401 | Missing or invalid token |
| 403 | Authenticated but not permitted (e.g. a private source without a grant) |
| 404 | Unknown id |
| 409 | Job not finished — results requested too early |
| 422 | Job failed; `error` explains why |
| 429 | Rate limited |
| 503 | Dependency unavailable (database) |

---

## Ops endpoints — **implemented**

Never token-gated: a probe cannot hold a secret.

### `GET /healthz`

Readiness. Returns 503 when Postgres is unreachable, so an unhealthy pod is
pulled from the load balancer rather than taking traffic it cannot serve.

```json
{"status": "ok"}
```

### `GET /api/v1/ping`

```json
{"pong": "ok"}
```

**Authenticated on purpose**, unlike `/healthz`. This is how a client checks that
its credential works, which an endpoint that answered without one could not tell
it. Use `/healthz` for liveness.

### `GET /version`

Liveness and build identification. Deliberately independent of the database — a
brief Postgres outage should drain pods, not restart them.

```json
{"version": "v0.1.0"}
```

---

## Catalog — **implemented**

### `GET /api/v1/snapshots`

Curated, versioned bundles. Feeds Step 1's snapshot cards.

```json
{
  "snapshots": [
    {
      "id": "clinical-v4",
      "name": "GRCh38 Clinical v4",
      "build": "GRCh38",
      "source_count": 6,
      "description": "Curated for germline variant review …",
      "tags": ["VEP·110", "ClinVar", "gnomAD·4.1", "SpliceAI"],
      "visibility": "signed_in",
      "constrained_by": ["gnomad:4.1 (signed_in)"],
      "contains_private": true,
      "state": "published",
      "pinned_at": 1780000000
    }
  ]
}
```

`visibility` is derived from the pinned sources rather than stored (see
[Visibility](#visibility)), and `constrained_by` names the ones holding it above
`public`. `contains_private` drives the card's lock notice.

The list is already filtered to what the caller may see — a snapshot pinning
sources they cannot use must not appear.

### `GET /api/v1/snapshots/{id}`

One snapshot with its **pinned source versions** (`snapshot.sources[]`, each
carrying `name`, `version` and `ref`), `contains_private`, and `annotations[]` —
every field its sources can contribute, each attributed to its source and flagged
`default` if the snapshot applies it. That list is what the flow's field picker
renders.

Fields are derived from each source's stored manifest on read, not kept as a
column: they are a projection of `toml_text` like the rest, and deriving them
means a source registered before this existed needs no backfill.

`GET /sources` carries the same `annotations[]` per source, so choosing individual
sources and choosing fields can happen in one step.

This is the reproducibility hook: a consumer that records "annotated under
snapshot X" needs to know which ClinVar and gnomAD releases that meant, so a count
can be reproduced later or diffed against a refresh.

### `GET /api/v1/sources`

Individual sources, for Step 1's table and the admin Sources tab.

```json
{
  "sources": [
    {
      "id": "clinvar",
      "name": "ClinVar",
      "detail": "Clinical significance",
      "versions": ["2026-06", "2026-03"],
      "format": "vcf.gz",
      "kind": "vcf",
      "visibility": "public",
      "index_status": "indexed",
      "origin": "registry: ncbi-clinvar"
    }
  ]
}
```

`kind` ∈ `vcf | bed | gtf | tsv | genelist | vep | builtin`.
`index_status` ∈ `indexed | building | error` — `building` is the state after
registration while an index is generated.

**As implemented**, rows are flat — one per `(name, version)`, each with a `ref`
of `"name:version"` — rather than grouped with a `versions[]` array. A consumer
pinning annotations must address an exact version, and grouping loses which
version a given snapshot pinned; the grouped view is a presentation concern the
SPA can derive.

---

## Annotation — **implemented**

### `POST /api/v1/annotate`

Submit variants. Rate limited per client IP.

```json
{
  "build": "GRCh38",
  "snapshot": "clinical-v4",
  "sources": ["clinvar", "gnomad"],
  "variants": ["chr17-7676154-C-T", "NM_000546.6:c.215C>G", "rs28934578"],
  "annotations": "all"
}
```

- Exactly one of `snapshot` or `sources` is required. With `sources`, `build` is
  also required.
- `variants` accepts VCF-style coordinates, HGVS, and rsIDs, mixed freely.
  **Only VCF-style coordinates are supported today**; HGVS and rsID resolution
  is Chunk 5 work and needs an authoritative transcript set (open question).
- `annotations` is optional: omit for the snapshot's defaults, `"all"`, a comma
  string, or an array of names.

`202` → `{"job_id": "…"}`.

Capped at **10,000 variants** per request. Loci reach the engine as argv, so a
larger batch would not fail cleanly — it trips the kernel's `ARG_MAX` and the exec
fails for a reason unrelated to the variants. Over the cap returns `413` pointing
at the VCF endpoint, which streams through a file and has no such ceiling.

An ad-hoc `sources` list **is** supported. The engine annotates against a
snapshot, so the selection is persisted as one — a real snapshot in state
`adhoc`, materialized and reproducible like any other, but never listed in
`GET /snapshots`. Its id is derived from the sorted source set plus the build, so
resubmitting the same selection reuses one row instead of accumulating a snapshot
per job.

Submission is always `202` with a `job_id`, whatever happens next: annotation is
asynchronous, so a submission's whole result is the identifier to follow it by.
Poll `GET /jobs/{id}` for status and fetch `GET /jobs/{id}/export` when it is
`done`.

### `POST /api/v1/annotate/vcf`

`multipart/form-data`. Fields: `vcf` (file, required), plus `build`, `snapshot`,
`sources`, `annotations` as above. Max upload 64 MiB — keep the ingress's
`proxy-body-size` in step.

Same responses as `/annotate`.

---

## Jobs — **implemented**

### `GET /api/v1/jobs`

Paginated list, newest first. Query: `status`, `limit` (default 50, max 500), `offset`, and `kind`.

`kind` defaults to `annotation` — a provisioning run has no variants and no
results table, so listing it beside someone's annotations is noise in the view
they came for. `kind=download` is the admin job log; `kind=all` is both.

```json
{
  "jobs": [
    {
      "job_id": "a7f32e…",
      "kind": "vcf",
      "label": "cohort_batch07.vcf.gz",
      "snapshot": "clinical-v4",
      "build": "GRCh38",
      "status": "running",
      "stage": "querying_sources",
      "percent": 68,
      "n_variants": 4812,
      "created_at": 1780000000,
      "started_at": 1780000005,
      "finished_at": 0
    }
  ],
  "limit": 50,
  "offset": 0,
  "total": 128,
  "scoped": true
}
```

`scoped: true` means the caller sees only their own jobs. `stage` ∈
`normalizing | querying_sources | running_vep | merging`, matching the Running
screen's checklist; `percent` is 0–100.

### `GET /api/v1/jobs/{id}`

One job, same shape as a list entry.

**Ownership is enforced.** A caller may read only their own jobs unless they hold
an admin grant. This is a deliberate change from the previous server, where
knowing a job id was sufficient to read it.

### `GET /api/v1/jobs/{id}/log`

What the run printed — the tail of the CLI's progress output.

```json
{"job_id": "…", "output": "varhub: annotating 1 loci (2 annotation(s) selected)\n…", "recorded": true}
```

Kept for runs that **succeed as well as fail**. The case with the most to explain
is a job that finished cleanly having annotated nothing: it is `done`, its result
set is empty, and only this says whether the sources were consulted and matched
nothing or were never consulted at all.

`recorded: false` means no output was stored — a job from before logs were kept,
or one that printed nothing. That is distinct from `output: ""`.

Ownership is enforced as it is for the job itself.

### `POST /api/v1/jobs/{id}/cancel`

Stops a job, returning the job and whether this call is what stopped it.

```json
{"job": {…}, "cancelled": true}
```

A job that had already finished returns `200` with `"cancelled": false` and a
`detail` saying so, rather than an error: the caller wanted it stopped and it is
stopped. Cancelling is gated by the same ownership rule as reading — someone who
can start work can already occupy a worker, and stopping it frees the slot they
are holding rather than taking anything from anyone else. An administrator can
cancel anything, which is what the system jobs view uses.

A cancelled job ends in status `cancelled`, not `error`: a deliberate stop is a
decision rather than a fault, and counting one as a failure would distort the
failure rate on the metrics page.

### `GET /api/v1/jobs/{id}/results`

One page of annotated variants plus the column definitions needed to render them.

Query: `page` + `per_page` (default 100, max 1000), or `limit` + `offset` — if
`limit` is present it wins. `sort` and `order=asc|desc`. `q` for a
case-insensitive substring search across annotation values and the locus text.

```json
{
  "columns": [
    {"key": "tstv", "label": "tstv", "type": "categorical",
     "source": "builtins", "source_ref": "builtins:1", "default": true}
  ],
  "rows": [
    {"chrom": "chr1", "pos": 115256529, "ref": "T", "alt": "C",
     "annotations": {"auto_id": "chr1_115256529_T_C", "tstv": "TS", "indel": null}}
  ],
  "total": 3, "limit": 100, "offset": 0
}
```

Columns are dynamic — they depend on the job's selection — and each carries the
`source` that produced it, which the results table renders as a tag. An
annotation with no value is `null`, never omitted.

**Columns are recorded per job, not read from the catalog at render time.** A
snapshot can be re-pinned after a job runs; a job's results must stay renderable
as they were computed, or a column would change meaning under a result that never
contained it.

`sort` accepts `idx` (the engine's output order, the default), `locus`
(chrom then pos), or any annotation key present in the results. An unknown key is
`400` rather than silently ignored. Annotation sorts are numeric when the value
parses as a number and textual otherwise, and empty values sort last in both
directions — an absent value is not "smallest".

`total` reflects the active search, not the whole result set, so the pager stays
honest under a filter.

Status codes: `409` while queued or running, `422` if the job failed.

### `GET /api/v1/jobs/{id}/export?format=json|tsv|csv`

Streams the **entire** matching set — not the current page — honoring the active
`q` and `sort`. Sends `Content-Disposition: attachment` with a filename derived
from the job id.

Rows are streamed, never buffered, so a large export does not have to fit in
memory. The consequence is that response headers are committed before the first
row: a database error mid-stream cannot be turned into a clean error response, so
it truncates the body and is logged server-side.

Delimited output puts `chrom,pos,ref,alt` first, then one column per entry in the
column model, in that order. Numbers are formatted plainly — a large integer
prints as `1200000`, not `1.2e+06`.

Selecting a subset of rows (`?selected=`) is not implemented.

---

## Admin — **partly implemented**

| Method | Path | Status |
|---|---|---|
| `POST` | `/api/v1/admin/sources/validate` | **implemented** — parses a manifest, writes nothing |
| `POST` | `/api/v1/admin/sources` | **implemented** — register/update from a manifest |
| `POST` | `/api/v1/admin/snapshots` | **implemented** — create/update with pinned sources |
| `POST` | `/api/v1/admin/snapshots/{id}/publish` | **implemented** |
| `PATCH` | `/api/v1/admin/sources/{id}` | not implemented (re-POST the manifest) |
| `DELETE` | `/api/v1/admin/sources/{id}` | **implemented** — refused with 409 while a snapshot pins it |
| `GET` | `/api/v1/admin/sources/{id}/config` | **implemented** — the stored TOML manifest |
| `GET` | `/api/v1/admin/sources/{id}/settings` | **implemented** — this deployment's settings for the source |
| `PUT` | `/api/v1/admin/sources/{id}/settings` | **implemented** — replaces them; see below |
| `PUT` | `/api/v1/admin/snapshots/{id}/sources` | **implemented** — replaces the pinned set, order preserved |
| `GET` | `/api/v1/admin/registries` | **implemented** |
| `POST` | `/api/v1/admin/registries` | **implemented** — validates by fetching once |
| `DELETE` | `/api/v1/admin/registries/{id}` | **implemented** (not the builtin default) |
| `GET` | `/api/v1/admin/registries/{id}/datasets` | **implemented** — live listing |
| `GET` | `/api/v1/admin/registries/{id}/fetch?ref=` | **implemented** — returns a manifest for review |
| `POST` | `/api/v1/admin/snapshots/{id}/duplicate` | not implemented |
| `GET` | `/api/v1/admin/metrics` | **implemented** — throughput, queue state, storage usage |
| `GET` | `/api/v1/admin/storage` | **implemented** — storage locations |
| `POST` | `/api/v1/admin/storage` | **implemented** — add an S3 bucket (paths come from config) |
| `DELETE` | `/api/v1/admin/storage/{id}` | **implemented** (not config-managed ones) |
| `GET` | `/api/v1/admin/files` | **implemented** — downloaded files and sizes; `?source=` / `?storage=` narrow it |
| `POST` | `/api/v1/admin/downloads` | **implemented** — queues a provisioning job |
| `GET`/`PUT` | `/api/v1/admin/grants` | not implemented (needs teams) |

### Source settings

What this deployment decides about a source, as opposed to what the source's
manifest says about itself. Stored apart from `toml_text` so re-fetching a
manifest from a registry cannot silently discard them.

```json
{"settings": {"annotation_prefix": "GENCODE_48_", "cache_setup": true}}
```

`annotation_prefix` renames the source's output fields. It is a **substitution,
not a prepend**: a manifest declares the prefix its names already carry — VEP
names every field `VEP_Allele` — so swapping in `VEP_113_` replaces `VEP_` rather
than stacking onto it. `"-"` means deliberately no prefix, which `""` cannot
express: empty falls through to whatever the manifest declared.

The point is running two versions of the same source side by side, `GENCODE_48_`
next to `GENCODE_47_`, without their columns colliding.

Two consequences worth knowing, because both were once bugs:

- Every listing that names annotations returns the **effective** names, already
  renamed. A listing showing manifest names while annotation emitted prefixed
  ones would hand out selections that cannot resolve, and the failure would
  surface much later as `default_annotations references unknown annotation`.
- Changing the prefix **rewrites the snapshot defaults** that named this source's
  fields, since those are stored denormalized as plain strings. Only defaults
  belonging to this source move: two sources can emit the same bare name, and
  rewriting the other one's on a string match would silently repoint it.

`cache_setup` archives a tool's setup output to the object store, so a machine
with an empty data directory unpacks it instead of re-running an install that
takes hours. It does nothing for a filesystem storage target, where the directory
is already where another machine would look.

### Provisioning

`POST /admin/downloads` queues a job that runs `varhub download` for a set of
**sources** into a storage location. Sources, not snapshots: a source is the unit
of data, and requiring it to belong to a snapshot first would mean a newly
registered source could not be downloaded until someone bundled it — which is
backwards, since you bundle sources you already have. The engine still needs a
manifest, so one is synthesized in the job's temp home; nothing is written to the
catalog, because a download is not a reproducibility claim the way an annotation
is. All selected sources must agree on an assembly, since one manifest states one. It rides the same queue as annotation rather
than running inline — a download can take hours and move gigabytes — so it gets
the same persistence, scheduling and error reporting, and shows up in
`GET /jobs` with `kind: "download"`.

Sources that compute their annotation from the variant itself carry
`needs_data: false` in `GET /sources` and are skipped here — there is nothing to
fetch. A selection containing only such sources is a 400 rather than a job that
would do nothing.

The worker takes the file inventory after the download, because only the worker
is guaranteed to have the storage volume mounted. Files are attributed to a
source by varhub's cache layout, `<root>/<name>/<version>/…`.

### Removing a source

`DELETE /admin/sources/{id}` unregisters a source and queues a cleanup job per
storage location holding its files, returning their job ids as `cleanup_jobs`.
Deletion is refused with `409` while any snapshot pins the source, naming them:
a snapshot is a reproducibility claim, and letting a member vanish would break
every future annotation against it.

Ad-hoc snapshots are exempt from that check. They are generated per submission
from an individual-source selection and are hidden from every listing, so
counting them would make a source permanently undeletable behind something the
caller cannot see or remove. They are regenerable — the same selection yields the
same id — and past results carry their own column model, so the ad-hoc rows are
deleted along with the source rather than blocking it.

Filesystem locations are declared by the deployment (`VHW_STORAGE_PATHS`) and
reconciled at startup; the API refuses to add one, since a path is meaningless
unless the worker mounts it. Object-store locations may be declared the same way
(`VHW_STORAGE_S3`, as `name=s3://bucket/prefix`) or added at runtime through the
API.

Both kinds are usable targets: `varhub download` writes to a bucket as readily
as to a directory, and annotation reads back from one with range requests, so no
local copy of the data is needed. `usable: false` now means only that the kind is
unrecognised.

The dev compose stack can run a local S3-compatible gateway for exercising this
— see `deploy/compose/.env.example`. It is behind a profile and off by default,
because most work does not need a bucket and production points at a real one.

**Authorization is not role-based yet.** There are no accounts, so every valid
token can administer. The routes sit under `/admin` so the eventual role gate has
one place to attach. Note a registered manifest is executed by `varhub` and can
name build recipes and container images — anyone who can register a source can
run code on a worker, so treat the token as an administrative credential.

`POST /admin/snapshots` creates a **draft** unless `"publish": true`. Drafts are
**selectable** in the annotation flow — a snapshot has to be usable before anyone
can judge whether it is worth publishing — and every entry carries its `state` so
a client can mark a draft as not-yet-fixed. `?state=published` narrows to fixed
ones.

**What publishing fixes is the pinned source versions, and only that.**
`POST /admin/snapshots` returns `409` if it would change a published snapshot's
pins. Title, description, tags and default fields stay editable via
`PATCH /admin/snapshots/{id}`, because a job records the annotations it actually
ran with — changing a default does not rewrite history, and freezing a title
would mean a typo could never be fixed.

`DELETE /admin/snapshots/{id}` works on a published snapshot too: publishing fixes
what a snapshot *means*, not that the row exists forever. Existing job results
stay readable — each job stores its own column model — but new annotation against
that name stops working.

The response is the snapshot re-read from the database, which is what proves the
pins resolved — a bad set would otherwise look accepted and fail at job time.

### Registries

A registry is a static `registry.toml` listing source and snapshot *configs* — the
same file `varhub registry` reads, so a registry published for the CLI works here
unchanged. Only the location is stored; the catalog is fetched live, because a
registry gains sources over time and a mirrored copy would quietly hide them.

`/fetch` returns an entry's manifest **for review** rather than registering it.
That is deliberate: the fragment is executed by `varhub` and can name build
recipes and container images, so it lands in the editor to be read before it is
adopted. Registering is still the same `POST /admin/sources` call.

A bare `ref` resolves to the entry the publisher marked `latest`. Versions are not
reliably sortable — semver `1.3`, dbSNP `b157`, dates — so choosing "the newest"
by string order would silently pick wrong.

Entry `file` paths are resolved against the manifest's directory and required to
stay under it, so a registry cannot point a fetch at another host or elsewhere on
the one it lives on. Registry URLs must be http(s). Note this is a guard rail, not
a security boundary: only an admin can add a registry, and an admin can already
run code on a worker via a build recipe.

The TOML manifest is the primary configuration mechanism; direct upload is
secondary because the files are large. Registration validates the TOML, checks
reachability and checksum, then enqueues download and indexing.

---

## Genome browser — *Chunk 7*

### `GET /api/v1/tracks?build=GRCh38`

Per-source data and index URLs for igv.js, filtered by the caller's grants.

```json
{
  "tracks": [
    {
      "id": "clinvar", "name": "ClinVar", "type": "variant", "format": "vcf",
      "url": "https://…/clinvar.vcf.gz?X-Amz-Signature=…",
      "index_url": "https://…/clinvar.vcf.gz.tbi?X-Amz-Signature=…",
      "expires_at": 1780003600
    }
  ]
}
```

Presigned S3 URLs, since igv.js issues its own range requests from the browser.
Two consequences: the bucket needs CORS configured for the app's origin, and the
expiry has to outlast a browsing session or tracks break mid-use.

---

## Open questions

Carried from the handoff and from implementation review:

1. ~~**Results storage.**~~ Settled: a `job_variant` table with a JSONB
   `annotations` column, written in the same transaction as the result blob. The
   blob is kept as the verbatim record of what the CLI produced; the rows are a
   derived projection for querying. Revisit if a single job ever holds enough
   variants that a per-key index matters.
2. **HGVS resolution** needs an authoritative transcript set when input is
   ambiguous.
3. **Failed-job detail** — partly settled. A job's `error` now carries the CLI's
   own `error:` line, which is written for humans ("genelist X: needs gtf = …")
   and is what a user needs to fix a misconfigured source. The ephemeral config
   path is redacted, and a message still carrying an absolute path — or stderr
   with no recognizable error line — stays the opaque "annotation failed" rather
   than risk describing server layout. Full stderr remains logs-only; per-variant
   failures and stage attribution are still open.
4. **Filter panel** — which fields are filterable, and whether presets are saved.
5. **Column manager** — persisted per user, per job, or per snapshot?
6. **Large VCF upload** — size ceiling and whether resumable upload is required.
   The current 64 MiB limit is inherited, not chosen.

## `GET /api/v1/admin/metrics`

Counters for the admin dashboard, in one response so the figures agree with each
other: reading them separately lets a job finish between two requests and produce
totals that do not add up.

```json
{
  "jobs": {
    "total": 412, "succeeded": 398, "failed": 14,
    "queued": 2, "running": 1, "oldest_queued_at": 1785600000,
    "variants": 91422, "last_24h": 37, "last_7d": 210
  },
  "sources": {"total": 5, "provisioned": 1, "streamed": 3, "builtin": 1, "pending": 0},
  "storage": [
    {"storage_id": "cfg-default", "name": "default", "kind": "path",
     "uri": "/mnt/storage", "bytes": 0, "files": 0, "sources": 0, "is_default": true},
    {"storage_id": "cfg-versitygw", "name": "versitygw", "kind": "s3",
     "uri": "s3://varhub-dev", "bucket": "varhub-dev",
     "bytes": 78704219, "files": 2, "sources": 1, "is_default": false}
  ],
  "storage_bytes": 78704219,
  "remote": [
    {"source_id": "gnomad-4.1.0", "name": "gnomAD", "host": "storage.googleapis.com",
     "files": 24, "bytes": 563049556499}
  ],
  "remote_bytes": 563049556499,
  "remote_measured": true,
  "generated_at": 1785605894
}
```

Notes on what the numbers mean, because several of them could reasonably be
defined another way:

- **`jobs.variants` counts successful jobs only.** A failed job's variant count is
  what was *submitted*, not what was annotated, so including it would inflate the
  total exactly when something is going wrong.
- **`oldest_queued_at` is absent when nothing is waiting.** A queue depth alone
  does not distinguish a queue that is moving from one that is stuck.
- **Every storage location appears, including empty ones.** "This bucket is
  configured and holds nothing" is usually the answer being looked for, and
  omitting the row makes it indistinguishable from a location that does not exist.
- **Locations are never merged.** Two locations in one bucket stay two rows, each
  carrying `bucket`, so usage can be read per location or summed per bucket.
- **`remote` is measured, not stored.** Streamed sources are read from their
  origin with range requests and occupy nothing here, so their bytes are reported
  separately and are *not* part of `storage_bytes`. Sizes come from a `HEAD` per
  file, falling back to a one-byte ranged `GET` for origins that refuse `HEAD`,
  cached for six hours.
- **`remote_measured: false` makes `remote_bytes` a floor.** Some origins report
  no length; the per-source `unmeasured` count says how many files are missing
  from the figure rather than quietly under-reporting.
- **`?remote=0`** skips the origin probes and returns the local figures alone.

## Authentication

Three credentials, in the order the middleware tries them. Whichever resolves
becomes the request's caller; nothing downstream authenticates separately.

| Credential | Header / cookie | Who it is |
| --- | --- | --- |
| Personal API token | `Authorization: Bearer cgl_vh_…` | the account that owns it |
| Session | `vh_auth` cookie, set by `POST /auth/login` | the account that signed in |
| Bootstrap token | `Authorization: Bearer cgl_vhb_…` | nobody — the first-administrator path |

There is no deployment-wide shared key. `VHW_MASTER_KEY` was removed: a shared
secret cannot be attributed to anyone, cannot be revoked without rotating it for
every holder at once, and is indistinguishable from a copy of itself. Scripts and
bulk loaders now hold a personal API token belonging to an account — often an
account created for that purpose — which is individually revocable and shows when
it was last used. Setting the variable is warned about at startup rather than
silently ignored.

An unrecognised credential is anonymous, not an error: a stale token gets the
same treatment as no token, so a client that kept one past its revocation sees a
sign-in prompt rather than a hard failure it cannot interpret.

### Administration is a property of the account

`/api/v1/admin/*` requires `role = admin`. A token administers only because the
person who owns it does — the role is read from the account on every request, so
promoting takes effect on tokens already issued and demoting revokes them all at
once, with nothing to reissue or clean up.

Reading *anyone's* jobs is likewise administrator-only.

**The submit rate limit applies to anonymous callers only.** It exists to stop an
unaccountable browser flooding the queue; an account is accountable — it can be
disabled and its jobs are attributable — and throttling a signed-in bulk load
would make that load throttle itself. The per-IP *concurrency* cap still applies
to everyone, so no one caller monopolises the workers.

### `GET /api/v1/auth/identities`

External identities linked to the calling account — the institutional logins that
can sign in as this user.

```json
{"identities": [{"provider": "cilogon", "subject": "http://cilogon.org/serverA/users/…", "email": "…"}]}
```

Scoped to the caller. An account with no external identity returns an empty list,
which is the ordinary case for a password account.

### Bootstrap: the first administrator

An installation with no accounts cannot be administered, and an administrator
cannot be created without administering. The service breaks that circle by
minting one bootstrap token at startup whenever no enabled administrator exists,
and logging it:

```
serve: this installation has no administrator yet.
serve:     cgl_vhb_ravjR2Fl5TP8abeRd7Onhz4_vAcqJHI8i1vjALxH0TI
```

It passes `requireAdmin` and nothing else, and it dies three ways: it is consumed
when it creates an administrator, it stops resolving the moment any enabled
administrator exists (so an unspent one is not a standing back door), and a
restart replaces it (so a token printed into a log and never used stops working).
`GET /auth/me` reports `needs_bootstrap` so the sign-in screen can ask for it.

After that, people sign in with an email and password.

### Changing a password

`POST /auth/password` takes `current_password` and `new_password`. The current
one is required even though the caller is already authenticated: a session cookie
and an API token are both bearer credentials, and if one is stolen, setting a new
password without knowing the old one would turn a read of the account into a
takeover. Re-proving knowledge of the password is what prevents that.

Succeeding ends the account's **other** sessions — someone changing a password
because they think it leaked expects that — while leaving API tokens alone, since
those are separate credentials with their own revocation and silently breaking a
CI job is not what the request asked for.

An account with no password stored here is refused with `409`. `GET /auth/me`
reports `can_change_password`, and a user carries `sso: true`, so the UI shows no
form rather than one that always fails. Hiding the form is courtesy; the server
check is the control.

### Personal API tokens

`POST /auth/tokens` returns `{"token": {...}, "secret": "cgl_vh_…"}`. The secret
is shown once and stored only as a SHA-256 hash, so a database leak yields no
working credentials. The `cgl_vh_` marker exists so a leaked token is greppable
by secret scanners, and the stored prefix locates the row without the hash being
reversible — presenting the prefix alone never authenticates.

One account holds as many tokens as it likes. Each is revoked on its own and
carries `last_used_at`, so a machine that is decommissioned costs one revocation
rather than a rotation of everything, and a token nobody has used is visibly safe
to remove. Revoked tokens stay listed: the row is the record that the token
existed and when it stopped working, which is what an audit of a leak needs.

### CILogon (institutional sign-in)

Configured with three variables; an incomplete set leaves sign-in
password-only rather than half-enabled.

| Variable | Purpose |
| --- | --- |
| `VHW_CILOGON_CLIENT_ID` | OIDC client id |
| `VHW_CILOGON_CLIENT_SECRET` | OIDC client secret |
| `VHW_CILOGON_REDIRECT_URL` | e.g. `https://varianthub.example/auth/cilogon/callback` |
| `VHW_CILOGON_AUTO_PROVISION_DOMAINS` | email domains that get an account on first sign-in; empty = invite-only |

Two browser routes, deliberately outside `/api/v1` so no JSON error wrapper
intercepts a redirect the provider has to follow:

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/auth/cilogon` | redirects to CILogon; `?next=/path` survives the round trip |
| `GET` | `/auth/cilogon/callback` | exchanges the code, issues `vh_auth`, redirects to `next` |

**Accounts are not created just because CILogon vouched for someone.** CILogon
federates several thousand institutional providers, so a successful login proves
only that *an* institution recognises the person — not that they belong on this
deployment. Resolution runs in three steps:

1. **By subject.** The provider's `sub` claim is the join key, not the email, so
   someone whose institutional address changes returns to the same account
   instead of acquiring a second one.
2. **By verified email.** An account an administrator already created is linked
   on first sign-in. This is the invitation: create the account with an empty
   password (it is then an SSO account with nothing to leak) and the first
   sign-in claims it.
3. **By allow-listed domain.** Only if `VHW_CILOGON_AUTO_PROVISION_DOMAINS`
   covers the verified address. A configured `iu.edu` also matches
   `umail.iu.edu`, because institutions routinely issue mail on a subdomain —
   but not `notiu.edu`. Auto-provisioned accounts are always **members**;
   administration is granted deliberately, never by having the right email.

Anything else is refused with `?error=sso_no_account`, which the sign-in screen
renders as "ask an administrator to add you". A disabled account is refused too:
SSO is not a way back in.

The `state` parameter is held in a short-lived, path-scoped, `HttpOnly` cookie
and compared on return, so another site's callback cannot be replayed into a
browser as a login. `?next=` is restricted to same-origin absolute paths —
`//host` is rejected because a browser reads it as protocol-relative and leaves
the site.

An account may hold a password *and* an identity, either alone. Unlinking the
last one is refused when there is no password, since that would leave an account
nobody can sign in to.

### Visibility

Every source, gene list and snapshot carries one of three levels:

| Level | Who |
|---|---|
| `public` | Anyone who can reach the server, anonymous visitors included |
| `signed_in` | Any account — no group membership needed |
| `restricted` | Members of a team it has been granted to |

A source is **`restricted` by default**. Grants attach to teams rather than to
people so access survives membership changes.

`signed_in` exists because the other two could not express the most common case.
"Not for anonymous visitors" is a property of the deployment rather than of each
dataset, and saying it through grants meant per-source administration that grew
with the catalog. A grant still works as a per-source exception at any level.

**Only sources carry a level.** A snapshot's is derived — the most restrictive of
everything it pins — and reported rather than set. A snapshot is a claim about
which annotations a result carries, so it can never be offered more widely than
the sources behind it; and a stored level could only agree with them or
contradict them, which is a way for an access decision to be quietly wrong with
two places to look for why.

The listing reports it as `visibility`, alongside `constrained_by` naming the
pinned sources that are not public. To change a snapshot's level, change a
source's, or pin different sources.

Changing a level is its own endpoint (`PUT .../visibility`) rather than a field on
the manifest editor. Editing a manifest is a statement about what a source *is*;
this is a statement about who it is for, and folding the second into the first
meant an unrelated one-line edit could close a source to everyone using it.

The name `private` is accepted on input and means `restricted`.

A snapshot pinning a source the caller cannot see is **hidden entirely** — absent
from the listing, `404` (not `403`) when fetched by name, and refused by
`POST /annotate`. All-or-nothing is deliberate: a snapshot is a claim about which
annotations a result carries, and returning it with a source quietly dropped
would answer a different question than the one asked, with nothing in the
response to say so. The `404` matters for the same reason a `403` would not — a
snapshot's name and existence are themselves information about what an
installation holds.

Selecting sources individually is checked the same way, so the ad-hoc path is not
a way around the snapshot rule.

### Job ownership

Jobs carry `user_id`, written from the verified credential. A job with an owner is
readable only by that account, by an administrator, or by the service account.
Jobs submitted anonymously still scope by the client-asserted `X-Varhub-Session`,
which is all an anonymous visitor has — but that header is never honoured for a
job that has an owner, or anyone who learned the string could read a signed-in
user's results.

### Configuration

| Variable | Default | Effect |
| --- | --- | --- |
| `VHW_ALLOW_ANONYMOUS` | `false` | let callers with no account use the annotation flow |

`VHW_MASTER_KEY` and `VHW_REQUIRE_TOKEN` were removed; both are warned about at
startup if still set.

### Endpoints

| Method | Path | Notes |
| --- | --- | --- |
| `POST` | `/api/v1/auth/login` | email + password → `vh_auth` cookie |
| `POST` | `/api/v1/auth/logout` | ends the session server-side |
| `POST` | `/api/v1/auth/password` | change your own password; `409` for an SSO account |
| `GET` | `/api/v1/auth/me` | caller, role, teams, `needs_bootstrap` |
| `GET` | `/api/v1/auth/tokens` | the caller's own tokens |
| `POST` | `/api/v1/auth/tokens` | mint one; the secret is in this response only |
| `DELETE` | `/api/v1/auth/tokens/{id}` | revoke one |
| `GET` | `/api/v1/admin/users` | accounts |
| `POST` | `/api/v1/admin/users` | create — also the bootstrap path |
| `PATCH` | `/api/v1/admin/users/{id}` | role, disabled, password |
| `GET` | `/api/v1/admin/teams` | teams with their members |
| `POST` | `/api/v1/admin/teams` | create |
| `DELETE` | `/api/v1/admin/teams/{id}` | delete; its grants go with it |
| `POST` | `/api/v1/admin/teams/{id}/members` | add a member |
| `DELETE` | `/api/v1/admin/teams/{id}/members/{user}` | remove one |
| `GET` | `/api/v1/admin/sources/{id}/grants` | teams that may see a restricted source |
| `POST` | `/api/v1/admin/sources/{id}/grants` | grant |
| `DELETE` | `/api/v1/admin/sources/{id}/grants/{team}` | revoke |
| `PUT` | `/api/v1/admin/sources/{id}/visibility` | set `public` \| `signed_in` \| `restricted` |
