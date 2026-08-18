-- The record outlives the payload.
--
-- The sweeper ran `DELETE FROM job` at job_ttl, and everything hangs off that
-- row by ON DELETE CASCADE — chunks, input, result, variants, chunk_log,
-- attempts. So
-- seven days after a job finished there was no evidence it had ever run: who
-- submitted it, against which snapshot, how many variants, whether it worked.
-- That is a record being destroyed to reclaim the space of a payload.
--
-- The two have different lifetimes and should have been separated from the
-- start. A job's *payload* is the submitted VCF, the annotated result, the rows
-- of the results table and the run log: large, regenerable in principle, and
-- nobody's business a month later. A job's *record* is that it happened — small,
-- not regenerable at all, and the thing an operator needs to answer "what has
-- this installation been doing" or "did that run ever finish".
--
-- So the sweep stops deleting the job and starts emptying it, stamping
-- purged_at on the way past. What is kept is a summary, not the machinery:
-- Marcus asked for the job record rather than its chunks or steps, so chunks are
-- purged with the rest and what they knew is frozen onto the job first.
--
-- Already-visible cost of the old behaviour: catalog.UsageWindows reports over
-- 7, 30 and 90 days from job_state, while the rows were deleted at 7. The two
-- longer windows could never exceed the shortest one and never had. They start
-- working when this lands, with no change to the reporting code.

-- The frozen summary.
--
-- Deliberately NOT maintained alongside the derived values. Migration 0035 was
-- right that a copy kept in step by every terminal path remembering to write it
-- is a second answer waiting to disagree with the first. These are written
-- exactly once, by the sweep, from the view, at the moment the rows behind the
-- view are about to stop existing. Before that instant the derivation is
-- authoritative and these are NULL; after it, there is nothing to disagree with.
ALTER TABLE job ADD COLUMN IF NOT EXISTS purged_at          BIGINT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS final_status       TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS final_finished_at  BIGINT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS final_n_variants   BIGINT;

-- What executed the job.
--
-- "local" is the pool of worker processes this deployment runs; "slurm" is the
-- cluster runner that does not exist yet. Recorded now rather than when it does,
-- because the value is only knowable while the work is running: a record written
-- later cannot say what ran something last year, and adding the column after the
-- fact makes every job that came before it indistinguishable from a local one.
--
-- On the chunk because that is what a worker claims and so what knows; on the
-- job because the chunk will not be there later.
ALTER TABLE chunk ADD COLUMN IF NOT EXISTS runner TEXT;
ALTER TABLE job   ADD COLUMN IF NOT EXISTS runner TEXT;

-- Finding the jobs whose payload is due, without scanning the ones already done.
CREATE INDEX IF NOT EXISTS job_unpurged ON job (id) WHERE purged_at IS NULL;

-- job_state answers from the chunks while they exist and from the summary once
-- they do not, so every reader of this view is unchanged by a purge.
--
-- The order matters and is not the obvious one. A purged job has no chunks, so
-- the aggregate reports n_chunks = 0 and n_done = 0 — and `n_done = n_chunks` is
-- then trivially true, which is the existing test for 'done'. A failed job whose
-- payload had expired would have reported success. So the frozen status is
-- consulted first, before any of the derived cases can fire.
CREATE OR REPLACE VIEW job_state AS
SELECT j.id,
       j.kind, j.snapshot, j.selection, j.label,
       j.client_ip, j.session_id, j.user_id, j.origin,
       j.created_at, j.prefix, j.input_chunk_id,
       CASE
         -- Frozen first: see above. This already encodes cancellation, having
         -- been taken from this same view.
         WHEN j.purged_at IS NOT NULL           THEN j.final_status
         WHEN j.cancelled_at IS NOT NULL        THEN 'cancelled'
         WHEN c.n_error > 0                     THEN 'error'
         WHEN c.n_done = c.n_chunks             THEN 'done'
         WHEN c.n_running > 0 AND c.n_done > 0  THEN 'partial_running'
         WHEN c.n_running > 0                   THEN 'running'
         WHEN c.n_done > 0                      THEN 'partial_queued'
         ELSE 'queued'
       END AS status,
       c.error,
       COALESCE(c.n_chunks, 0) AS chunks,
       COALESCE(c.n_done, 0)   AS done,
       COALESCE(c.n_error, 0)  AS failed,
       c.started_at,
       COALESCE(
         CASE
           WHEN j.cancelled_at IS NOT NULL THEN j.cancelled_at
           WHEN c.n_error > 0 OR c.n_done = c.n_chunks THEN c.finished_at
         END,
         j.final_finished_at) AS finished_at,
       COALESCE(c.n_variants, j.final_n_variants, 0) AS n_variants,
       c.result_chunk_id,
       c.columns,
       -- Appended rather than placed where they read best. CREATE OR REPLACE
       -- VIEW may only add columns at the end — inserting one renames the column
       -- that was in that position, and Postgres refuses. Dropping the view
       -- instead would allow any order at the cost of a window where anything
       -- reading it fails, which is not worth a tidier column list.
       j.purged_at,
       COALESCE(c.runner, j.runner) AS runner
  FROM job j
  LEFT JOIN LATERAL (
    SELECT count(*)                                     AS n_chunks,
           count(*) FILTER (WHERE k.status = 'done')    AS n_done,
           count(*) FILTER (WHERE k.status = 'error')   AS n_error,
           count(*) FILTER (WHERE k.status = 'running') AS n_running,
           min(k.started_at)                            AS started_at,
           max(k.finished_at)                           AS finished_at,
           (SELECT r.runner FROM chunk r
             WHERE r.job_id = j.id AND r.runner IS NOT NULL
             ORDER BY r.chunk_index NULLS FIRST, r.id LIMIT 1) AS runner,
           COALESCE(
             sum(k.n_variants) FILTER (WHERE k.chunk_index IS NOT NULL),
             sum(k.n_variants) FILTER (WHERE k.completes_job)
           ) AS n_variants,
           (SELECT e.error FROM chunk e
             WHERE e.job_id = j.id AND e.status = 'error' AND e.error IS NOT NULL
             ORDER BY e.finished_at, e.chunk_index NULLS FIRST, e.id LIMIT 1) AS error,
           (SELECT r.id FROM chunk r
             WHERE r.job_id = j.id AND r.completes_job AND r.status = 'done'
             LIMIT 1) AS result_chunk_id,
           (SELECT n.columns FROM chunk n
             WHERE n.job_id = j.id AND n.columns IS NOT NULL
             ORDER BY n.completes_job DESC, n.chunk_index NULLS FIRST LIMIT 1) AS columns
      FROM chunk k WHERE k.job_id = j.id
  ) c ON TRUE;
