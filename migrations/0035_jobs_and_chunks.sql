-- Split the queue's one entity into two: a job is what a caller submits, a
-- chunk is what a worker runs.
--
-- Until here they were the same row, which worked only while every submission
-- was one unit of work. A VCF past the chunk size is not: chromosome 22 of a
-- WGS cohort is 2.6M variants, and running that as one unit means one process
-- holding one worker for hours with no progress to report and everything lost
-- if it dies. It is cut into pieces that annotate independently and are joined
-- at the end, and the thing the caller holds has to be the whole submission
-- rather than any one piece of it.
--
--   job    what a caller submits and polls. Every submission is one, with at
--          least one chunk. A locus list is a job of one; a chromosome is a
--          job of twenty-eight — a split, twenty-six pieces, and the collect
--          that joins them.
--   chunk  what a worker claims, leases, retries and abandons.
--
-- This renames rather than recreates, so the queue keeps its indexes, its
-- constraints and its shape — everything below the caller's handle is the same
-- table it always was. It assumes an empty queue: chunk.job_id is NOT NULL and
-- nothing backfills it, so a database with rows in flight will fail here rather
-- than invent a job for them. That is deliberate and was agreed: this is a
-- reshaping, not a data migration.

-- 1. The unit a worker runs, and everything hanging off it.
ALTER TABLE job_input   RENAME TO chunk_input;
ALTER TABLE job_result  RENAME TO chunk_result;
ALTER TABLE job_variant RENAME TO chunk_variant;
ALTER TABLE job_log     RENAME TO chunk_log;
ALTER TABLE job_attempt RENAME TO chunk_attempt;

ALTER TABLE chunk_input   RENAME COLUMN job_id TO chunk_id;
ALTER TABLE chunk_result  RENAME COLUMN job_id TO chunk_id;
ALTER TABLE chunk_variant RENAME COLUMN job_id TO chunk_id;
ALTER TABLE chunk_log     RENAME COLUMN job_id TO chunk_id;
ALTER TABLE chunk_attempt RENAME COLUMN job_id TO chunk_id;

-- Renamed before the new job table is created, or the name collides.
ALTER TABLE job RENAME TO chunk;

-- Its place in a split, counting from zero. NULL for a chunk that is not a
-- piece of one — the sole chunk of an ordinary submission, and the split and
-- collect that bracket a fan-out. Nullable rather than defaulted because 0 is a
-- real index: chunk 0 carries the header, so "the first piece" and "not a
-- piece" have to be different values.
ALTER TABLE chunk ADD COLUMN chunk_index INTEGER;

-- This chunk runs only once every piece of its job has finished.
--
-- The collect that joins a split submission, and nothing else. It is queued by
-- the split, at the same time as the pieces, rather than by whichever piece
-- happens to finish last — so the work a job still owes is visible as rows from
-- the moment there is any, instead of being an intention held by whichever
-- worker gets there.
--
-- That is what makes a job's state readable from its chunks. With the collect
-- queued at the end there was a moment when every chunk that existed was
-- terminal and no answer had been assembled, which anything counting rows would
-- read as finished. Here the collect is one of the rows being counted.
--
-- It also removes the need for a counter to decide who starts the collect.
-- Exactly one worker claims it, because exactly one worker claims anything —
-- see the claim's advisory lock.
ALTER TABLE chunk ADD COLUMN awaits_pieces BOOLEAN NOT NULL DEFAULT FALSE;

-- Does finishing this chunk produce the job's answer?
--
-- True for the sole chunk of an ordinary submission and for the collect that
-- joins a split one. Not a correctness guard — "all of them are done" is what
-- makes a job done — but the record of which chunk's stored output is the one
-- to serve.
ALTER TABLE chunk ADD COLUMN completes_job BOOLEAN NOT NULL DEFAULT FALSE;

-- 2. The submission.
--
-- Note what is not here: status, error, finished_at, n_variants, the result.
-- Chunks are the unit of execution, so those are facts about the chunks and are
-- read from them — see the job_state view below. A copy on this row would be a
-- second answer to the same question, kept in step by every terminal path
-- remembering to write it, and the first one that forgot would leave a
-- submission reporting the wrong thing with nothing to compare against.
CREATE TABLE job (
  id         TEXT PRIMARY KEY,

  -- What was asked for. Copied onto each chunk as well rather than joined at
  -- dispatch: the claim query stays one statement over one table, and the
  -- scheduler's terms stay on the row it schedules.
  kind       TEXT NOT NULL,
  snapshot   TEXT NOT NULL,
  selection  TEXT NOT NULL DEFAULT '',
  label      TEXT,
  client_ip  TEXT,
  session_id TEXT,
  user_id    TEXT,
  origin     TEXT,
  created_at BIGINT NOT NULL,

  -- When someone asked for it to stop.
  --
  -- The one piece of state here that is not derivable, because it is not a fact
  -- about the chunks: it is an instruction that arrived from outside. A queued
  -- chunk is settled the moment it is given, but a running one is only
  -- signalled — its worker records the outcome when the message reaches it —
  -- and a caller who cancels and then polls must not be told the work is still
  -- running in the meantime.
  cancelled_at BIGINT,

  -- Where a split's pieces live: <job_storage>/jobs/<job-id>. NULL for a job
  -- that was never split, which owns no pieces to put anywhere.
  prefix TEXT,

  -- The chunk created with the job, holding the submitted input: the locus
  -- list, or the uploaded VCF a split cuts up. An export that merges
  -- annotations back onto what was sent reads it from there.
  --
  -- Written once at submit and never again, so it cannot drift. The chunk that
  -- holds the *answer* is not here: that one changes as the job runs, so it is
  -- derived rather than written.
  --
  -- Deliberately not a foreign key. chunk.job_id below cascades from job, and a
  -- key in the other direction would make the pair a cycle a delete has to be
  -- ordered around for no benefit.
  input_chunk_id TEXT
);

-- 3. The link, and the cascade. A job's TTL sweep removes the job; its chunks
-- go with it, and their inputs, results, variants, logs and attempts with them.
ALTER TABLE chunk ADD COLUMN job_id TEXT NOT NULL
  REFERENCES job (id) ON DELETE CASCADE;

-- The pieces of one job, in order. Read by collect, which must join them in the
-- order they were split or the output is a VCF whose records go backwards; by
-- the claim, deciding whether a waiting collect may run; and by the view below,
-- which reads every chunk of a job on every job read.
CREATE INDEX chunk_by_job ON chunk (job_id, chunk_index);

CREATE INDEX job_by_session ON job (session_id, created_at DESC);
CREATE INDEX job_by_owner   ON job (user_id, created_at DESC);

-- 4. A job's state, read from its chunks.
--
-- One definition, used by every read: the status endpoint, the listing, the
-- metrics and the TTL sweep. Written out four times in Go it would be four
-- definitions, and the one that drifted would be whichever is exercised least.
--
-- The status vocabulary is the chunk's five plus two:
--
--   queued           nothing has started
--   running          something is running, nothing has finished
--   partial_running  something has finished, something is running
--   partial_queued   something has finished, nothing is running yet
--   done             every chunk finished
--   error            a chunk failed; there is no answer to assemble
--   cancelled        someone asked it to stop
--
-- "partial" is a property of a thing made of parts, so a chunk never has one.
-- They are what a fan-out actually looks like from outside: nine of twenty-six
-- pieces annotated is neither "running" nor "queued" in any useful sense, and
-- reporting it as either is why a caller ends up counting chunks themselves.
CREATE VIEW job_state AS
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
       -- A finish time only once there is nothing left to happen. A max over
       -- chunks that are still running would report the moment the last one to
       -- report happened to report.
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
           -- The pieces, or the sole chunk. SUM over no rows is NULL, which is
           -- what lets one expression serve a job that was split and one that
           -- was not.
           COALESCE(
             sum(k.n_variants) FILTER (WHERE k.chunk_index IS NOT NULL),
             sum(k.n_variants) FILTER (WHERE k.completes_job)
           ) AS n_variants,
           -- The first failure is the one that explains the job. A later chunk
           -- failing because the run was already coming apart is not the
           -- answer to "why did this fail".
           (SELECT e.error FROM chunk e
             WHERE e.job_id = j.id AND e.status = 'error' AND e.error IS NOT NULL
             ORDER BY e.finished_at, e.id LIMIT 1) AS error,
           (SELECT r.id FROM chunk r
             WHERE r.job_id = j.id AND r.completes_job AND r.status = 'done'
             LIMIT 1) AS result_chunk_id,
           -- Every chunk of a job describes the same columns — they annotated
           -- one file against one snapshot — so any that has them will do, and
           -- the one that produced the answer first.
           (SELECT n.columns FROM chunk n
             WHERE n.job_id = j.id AND n.columns IS NOT NULL
             ORDER BY n.completes_job DESC, n.chunk_index NULLS FIRST LIMIT 1) AS columns
      FROM chunk k WHERE k.job_id = j.id
  ) c ON TRUE;
