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

Chunk 1 issues a single HMAC token signed with `VHW_MASTER_KEY`. Chunk 6 replaces
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
      "contains_private": false,
      "state": "published",
      "pinned_at": 1780000000
    }
  ]
}
```

`contains_private` drives the card's lock notice. The list is already filtered to
what the caller may see — a snapshot whose private sources they lack grants for
must not appear.

### `GET /api/v1/snapshots/{id}`

One snapshot with its **pinned source versions** (`snapshot.sources[]`, each
carrying `name`, `version` and `ref`), plus `contains_private`.

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

- Exactly one of `snapshot` or `sources` is required.
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

An ad-hoc `sources` list is **not** implemented: the engine selects sources
through a snapshot. Supplying `sources` returns `400` rather than silently
annotating under something else.

`?wait=<seconds|duration>` blocks up to a server-capped interval
(`VHW_SUBMIT_WAIT_CAP`, default 10s) so fast jobs return inline. On completion
within the window the response is a `200` job object with `results` embedded.

### `POST /api/v1/annotate/vcf`

`multipart/form-data`. Fields: `vcf` (file, required), plus `build`, `snapshot`,
`sources`, `annotations` as above. Max upload 64 MiB — keep the ingress's
`proxy-body-size` in step.

Same responses as `/annotate`.

---

## Jobs — **implemented**

### `GET /api/v1/jobs`

Paginated list, newest first. Query: `status`, `limit` (default 50, max 500),
`offset`.

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

## Admin — *Chunk 5/6, role-gated*

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/admin/sources` | Validate and register a source from a TOML manifest |
| `PATCH` | `/api/v1/admin/sources/{id}` | Edit a source |
| `GET` | `/api/v1/admin/registries` | List configured public registries |
| `GET` | `/api/v1/admin/registries/{id}/datasets` | Available datasets in a registry |
| `POST` | `/api/v1/admin/snapshots` | Create a draft snapshot with pinned versions |
| `POST` | `/api/v1/admin/snapshots/{id}/publish` | Publish (making it immutable) |
| `POST` | `/api/v1/admin/snapshots/{id}/duplicate` | Copy to a new draft |
| `GET`/`PUT` | `/api/v1/admin/grants` | Read/write the team × private-source matrix |

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
