# VariantHub REST API (`/api/v1`)

The contract the React app is built against. Derived from the design handoff's
"Data the frontend needs from the backend" (`design_handoff_varianthub/README.md`).

**Implementation status.** Chunk 1 ships the router, auth, throttling, and the ops
endpoints. Everything under `/api/v1` below is *specified but not yet implemented*
— it lands in Chunk 5, once the Postgres catalog (Chunk 2) provides multiple
snapshots to serve. Each endpoint is marked accordingly. This document is the
agreed shape so front-end work can proceed in parallel; treat a change here as a
change to a shared contract.

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

## Catalog — *Chunk 5*

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

---

## Annotation — *Chunk 5*

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

`?wait=<seconds|duration>` blocks up to a server-capped interval
(`VHW_SUBMIT_WAIT_CAP`, default 10s) so fast jobs return inline. On completion
within the window the response is a `200` job object with `results` embedded.

### `POST /api/v1/annotate/vcf`

`multipart/form-data`. Fields: `vcf` (file, required), plus `build`, `snapshot`,
`sources`, `annotations` as above. Max upload 64 MiB — keep the ingress's
`proxy-body-size` in step.

Same responses as `/annotate`.

---

## Jobs — *Chunk 5*

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

Paginated, sorted, filtered rows plus the column definitions to render them.
Query: `page`, `per_page`, `sort`, `order`, `q`, and per-column filters.

```json
{
  "columns": [
    {"key": "gene",        "label": "Gene",       "source": "VEP",     "type": "text"},
    {"key": "clinvar_sig", "label": "ClinVar",    "source": "ClinVar", "type": "significance"},
    {"key": "gnomad_af",   "label": "gnomAD AF",  "source": "gnomAD",  "type": "number", "align": "right"}
  ],
  "rows": [
    {
      "chrom": "chr17", "pos": 7676154, "ref": "C", "alt": "T",
      "annotations": {"gene": "TP53", "clinvar_sig": "Pathogenic", "gnomad_af": null}
    }
  ],
  "page": 1, "per_page": 100, "total": 4812
}
```

Columns are dynamic — they depend on the sources the job selected — and each
carries the `source` that produced it, which the results table renders as a tag.
An annotation with no value is `null`, never omitted.

Status codes: `409` while queued or running, `422` if the job failed.

### `GET /api/v1/jobs/{id}/export?format=json|tsv|csv`

Streams the **entire** result set (not the current page), honoring active
filters. `?selected=<ids>` limits it to checkbox-selected rows.

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

1. **Results storage.** Pagination, sorting, and filtering cannot be served from
   one opaque JSON blob, which is how results are stored today. A `job_variant`
   table or a queryable JSONB column has to be chosen *before* the results
   handler is written.
2. **HGVS resolution** needs an authoritative transcript set when input is
   ambiguous.
3. **Failed-job detail** — what the error view shows (stage, stderr, per-variant
   failures). Note the worker deliberately does not put CLI stderr in the job's
   stored error; exposing it needs an explicit, admin-gated channel.
4. **Filter panel** — which fields are filterable, and whether presets are saved.
5. **Column manager** — persisted per user, per job, or per snapshot?
6. **Large VCF upload** — size ceiling and whether resumable upload is required.
   The current 64 MiB limit is inherited, not chosen.
