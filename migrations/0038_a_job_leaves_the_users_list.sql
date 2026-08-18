-- Deleting a job removes it from the person's list, not from the record.
--
-- Two different things get called "delete" and only one of them is on offer.
--
-- What a caller wants is their data gone: the VCF they uploaded, the annotated
-- result, the rows behind the results table. That is theirs to remove and it
-- should not wait seven days for the sweeper. It is exactly what migration 0037
-- already does on a timer, so a delete is that purge, asked for early.
--
-- What a caller does not get is the record. This installation keeps an account
-- of every job that ran — who, when, against which snapshot, how many variants,
-- how it ended — and that is not a user-editable list. An operator asking "what
-- has this been doing" must not get an answer shaped by who has been tidying up
-- after themselves, and usage reporting that a caller could delete from is not
-- reporting.
--
-- So deleted_at is a listing concern, not a lifecycle one. It says the caller
-- has finished with this job; the row stays exactly where it was.
ALTER TABLE job ADD COLUMN IF NOT EXISTS deleted_at BIGINT;

-- The listing is the query that has to care, and it is the common one — every
-- poll of /jobs runs it — so the index carries the predicate rather than making
-- each query filter after the fact.
CREATE INDEX IF NOT EXISTS job_live_by_session
  ON job (session_id, created_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS job_live_by_user
  ON job (user_id, created_at) WHERE deleted_at IS NULL;

-- Appended, because CREATE OR REPLACE VIEW may only add columns at the end.
-- Inserting one renames whatever held that position and Postgres refuses.
CREATE OR REPLACE VIEW job_state AS
SELECT j.id,
       j.kind, j.snapshot, j.selection, j.label,
       j.client_ip, j.session_id, j.user_id, j.origin,
       j.created_at, j.prefix, j.input_chunk_id,
       CASE
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
       j.purged_at,
       COALESCE(c.runner, j.runner) AS runner,
       j.deleted_at
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
