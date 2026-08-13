-- Make the job the thing a caller submits, and give every submission one.
--
-- 0036 renamed the entities; this makes them true. A job row existed only for a
-- split, so the id a caller held was still a chunk id and /jobs/{id} still
-- answered with a chunk — which is only coherent while every submission happens
-- to be one chunk, and stopped being true the moment VCFs started splitting.
--
-- After this: a submission inserts one job and at least one chunk. The job
-- carries what was submitted and how far it has got; the chunk carries what a
-- worker claims, leases and retries. /jobs/{id} reads a job.
--
-- The id a caller holds is the job's. For rows that already exist it is the
-- chunk's id, backfilled below onto the job, so every link and bookmark that
-- was handed out keeps resolving. New submissions mint a job id and give each
-- chunk its own; the two are no longer the same string, which is the point.

-- What was submitted. Copied onto the chunk as well rather than joined at
-- dispatch: the claim query stays one statement over one table, and the
-- scheduler's terms stay on the row it schedules.
ALTER TABLE job ADD COLUMN IF NOT EXISTS kind       TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS snapshot   TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS selection  TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS label      TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS client_ip  TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS session_id TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS user_id    TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS origin     TEXT;

-- How far it has got.
--
-- Written, not derived. Derived would mean every status poll aggregating over
-- a job's chunks, and — worse — a window where a split job's annotation chunks
-- have all finished but the collect that joins them has not been queued yet,
-- in which case "every chunk is terminal" reads as done and the caller fetches
-- a result that does not exist. A chunk says whether finishing it finishes the
-- job (see chunk.completes_job); nothing else moves a job to done.
ALTER TABLE job ADD COLUMN IF NOT EXISTS status      TEXT NOT NULL DEFAULT 'queued';
ALTER TABLE job ADD COLUMN IF NOT EXISTS error       TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS n_variants  BIGINT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS started_at  BIGINT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS finished_at BIGINT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS columns     TEXT;

-- The two chunks a job singles out.
--
-- input_chunk_id is the chunk created with the job, which holds the submitted
-- input: the locus list, or the uploaded VCF that a split cuts up. An export
-- that merges annotations back onto what was sent reads it from there.
--
-- result_chunk_id is the chunk whose stored result is the job's answer. For a
-- one-chunk job that is the same chunk; for a split one it is the collect.
-- Written when that chunk finishes, so a job that is not done has none.
--
-- Deliberately not foreign keys. chunk.job_id below cascades from job, and a
-- key in the other direction would make the pair a cycle that a delete has to
-- be ordered around for no benefit — nothing reads these for a job that is
-- being deleted.
ALTER TABLE job ADD COLUMN IF NOT EXISTS input_chunk_id  TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS result_chunk_id TEXT;

-- prefix is where a split's pieces live, so only a split has one.
ALTER TABLE job ALTER COLUMN prefix DROP NOT NULL;

-- chunk_id said "the chunk the submitter polls". They poll the job now.
ALTER TABLE job DROP COLUMN IF EXISTS chunk_id;

-- Does finishing this chunk finish its job?
--
-- True for the single chunk of an unsplit submission and for the collect that
-- joins a split one; false for a split, and for each piece it produced. This is
-- what stops a job going done between its last piece finishing and the collect
-- being queued.
ALTER TABLE chunk ADD COLUMN IF NOT EXISTS completes_job BOOLEAN NOT NULL DEFAULT FALSE;

-- Every chunk that predates this belonged to nothing. Give each one a job with
-- its own id, so the id its submitter was given still names something.
INSERT INTO job (id, kind, snapshot, selection, label, client_ip, session_id,
                 user_id, origin, status, error, n_variants, started_at,
                 finished_at, columns, input_chunk_id, result_chunk_id,
                 chunks, done, failed, created_at)
SELECT c.id, c.kind, c.snapshot, c.selection, c.label, c.client_ip,
       c.session_id, c.user_id, c.origin, c.status, c.error, c.n_variants,
       c.started_at, c.finished_at, c.columns, c.id,
       CASE WHEN c.status = 'done' THEN c.id END,
       1,
       CASE WHEN c.status = 'done' THEN 1 ELSE 0 END,
       CASE WHEN c.status = 'error' THEN 1 ELSE 0 END,
       c.created_at
  FROM chunk c
 WHERE c.job_id IS NULL;

UPDATE chunk SET job_id = id, completes_job = TRUE WHERE job_id IS NULL;

-- Now that every chunk has one, the link can be required and the delete can
-- cascade: a job's TTL sweep removes the job, and its chunks — with their
-- inputs, results, variants, logs and attempts — go with it.
ALTER TABLE chunk ALTER COLUMN job_id SET NOT NULL;
ALTER TABLE chunk ADD CONSTRAINT chunk_job_fk
  FOREIGN KEY (job_id) REFERENCES job (id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS job_by_session ON job (session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS job_by_owner   ON job (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS job_finished   ON job (finished_at)
  WHERE finished_at IS NOT NULL;
