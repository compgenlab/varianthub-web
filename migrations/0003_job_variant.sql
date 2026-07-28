-- Annotated variants as queryable rows, so results can be paginated, sorted and
-- filtered server-side. Until now a job's results were one opaque JSON blob,
-- which can serve a whole-result download and nothing else.
--
-- job_result.json is KEPT alongside this. It is the verbatim bytes varhub
-- produced, which is what makes "the API returns exactly what the CLI computed"
-- true, and it is what the inline ?wait= path returns without touching this
-- table. These rows are a derived projection of it, not a replacement.
--
-- Annotations are JSONB rather than an EAV table. At the scale the design
-- targets — thousands of variants per job, GC'd after 24h — a filtered scan of
-- one job's rows is a few thousand rows, which Postgres handles without an
-- index. An EAV table would multiply row count by the annotation count for
-- lookups that are not the bottleneck, and would still need a type-aware value
-- split to sort numerically. JSONB keeps one row per variant and lets the
-- column set stay dynamic per job, which it is.

CREATE TABLE IF NOT EXISTS job_variant (
  job_id      TEXT   NOT NULL REFERENCES job (id) ON DELETE CASCADE,
  idx         INT    NOT NULL,          -- position in the CLI's output; the default sort
  chrom       TEXT   NOT NULL,
  pos         BIGINT NOT NULL,
  ref         TEXT   NOT NULL,
  alt         TEXT   NOT NULL,
  annotations JSONB  NOT NULL,
  PRIMARY KEY (job_id, idx)
);

-- Sorting by genomic position is the common non-default ordering.
CREATE INDEX IF NOT EXISTS job_variant_locus ON job_variant (job_id, chrom, pos);

-- Containment/existence queries on annotation keys (e.g. "has a ClinVar call").
CREATE INDEX IF NOT EXISTS job_variant_ann ON job_variant USING GIN (annotations);

-- The column model as it was when the job ran: name, type, and the source that
-- produced each value, from `varhub annotation list --format json`.
--
-- Stored per job rather than read from the catalog at render time because a
-- snapshot's sources can be re-pinned afterwards. A job's results must stay
-- renderable as they were computed, or a column would change meaning under a
-- result that never contained it.
ALTER TABLE job ADD COLUMN IF NOT EXISTS columns JSONB;
