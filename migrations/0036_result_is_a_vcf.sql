-- The result blob leaves Postgres.
--
-- chunk_result.json held the engine's output verbatim: a JSON array of every
-- variant with every annotation on it, in a single row. For a locus lookup that
-- is a few kilobytes. For a chromosome of a WGS cohort it is gigabytes in one
-- column, TOASTed out of line, written once and — as it turns out — read never.
-- Nothing outside the queue's own tests has read it since chunk_variant was
-- added in 0003, which projected the same data into rows so it could be paged,
-- searched and sorted.
--
-- What replaces it is the file. Every job now stores its answer as a VCF under
-- its own prefix (see vcf_uri, added in 0034), and the tab, csv and json exports
-- are conversions of that object rather than second renderings from these rows.
-- One answer, in one place, in the format the data is actually in.
--
-- The row stays: it is what carries vcf_uri, which is the pointer to that file.
-- Only the blob goes.
--
-- One thing genuinely leaves with it. A provisioning chunk stored the manifest
-- of files it downloaded here, "so the UI can show it without a second call" —
-- but no caller ever made that call either, and the durable record is
-- source_file, which the same worker writes through catalog.ReplaceSourceFiles
-- before it returns. The manifest was a second copy of that, and the second copy
-- is the one that could go stale.
ALTER TABLE chunk_result DROP COLUMN json;

-- And, while the view is being touched: make "the first failure" mean something
-- when several fail at once.
--
-- job_state picks the error that explains a job by ordering failures on
-- finished_at. That is a Unix second, and the case with more than one failure is
-- a fan-out where every piece fails together — one missing reference FASTA and
-- twenty-six pieces fail inside the same second. The tiebreak was the chunk id,
-- which is random, so two identical runs reported different errors and the one
-- reported was rarely the one that happened first.
--
-- chunk_index is the order the work was in. Within a second, piece 3's failure
-- is the one to report, not whichever id sorted lowest. NULLS FIRST puts the
-- split ahead of the pieces, which is also when it ran.
CREATE OR REPLACE VIEW job_state AS
SELECT j.id,
       j.kind, j.snapshot, j.selection, j.label,
       j.client_ip, j.session_id, j.user_id, j.origin,
       j.created_at, j.prefix, j.input_chunk_id,
       CASE
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
       CASE
         WHEN j.cancelled_at IS NOT NULL THEN j.cancelled_at
         WHEN c.n_error > 0 OR c.n_done = c.n_chunks THEN c.finished_at
       END AS finished_at,
       COALESCE(c.n_variants, 0) AS n_variants,
       c.result_chunk_id,
       c.columns
  FROM job j
  LEFT JOIN LATERAL (
    SELECT count(*)                                     AS n_chunks,
           count(*) FILTER (WHERE k.status = 'done')    AS n_done,
           count(*) FILTER (WHERE k.status = 'error')   AS n_error,
           count(*) FILTER (WHERE k.status = 'running') AS n_running,
           min(k.started_at)                            AS started_at,
           max(k.finished_at)                           AS finished_at,
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
