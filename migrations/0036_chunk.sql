-- Rename the queue's two entities so the schema speaks the API's language.
--
-- Nothing about the data changes. What changes is which word means what:
--
--   job    is what a caller submits and polls. Every submission is one, with at
--          least one chunk. This was `batch`.
--   chunk  is the unit a worker claims, leases, retries and abandons. This was
--          `job`.
--
-- The old spelling had two names for one idea and one name for two: a "job" was
-- both the thing a caller holds and the thing a worker runs, which is why a
-- split submission needed a second entity — `batch` — to be the first of those
-- without being the second. Naming them apart is what removes the need for a
-- parallel set of endpoints over batches.
--
-- Renames only. Postgres rewrites no rows for these, and the foreign keys,
-- indexes and defaults follow their tables automatically. Index and constraint
-- names are left as they were: they appear in no query, and renaming them would
-- make this migration long enough to hide what it actually does.
--
-- Order matters. `job` has to become `chunk` before `batch` can become `job`,
-- or the second rename collides with a table that still exists.

-- 1. The satellite tables. Each hangs off the unit a worker runs, so each
--    follows it from job_* to chunk_*.
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

-- 2. The unit a worker claims.
--
-- batch_id becomes job_id: a chunk's owner is now spelled the way every other
-- child of it is.
ALTER TABLE job RENAME TO chunk;
ALTER TABLE chunk RENAME COLUMN batch_id TO job_id;

-- 3. The submission. Its job_id pointed at the row the submitter polls, which
--    is a chunk now — the split chunk, the one that exists before any other
--    does. Renaming the column keeps that fact readable instead of leaving a
--    job.job_id that points somewhere else.
ALTER TABLE batch RENAME TO job;
ALTER TABLE job RENAME COLUMN job_id TO chunk_id;
