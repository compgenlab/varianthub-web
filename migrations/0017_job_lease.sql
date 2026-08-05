-- Who holds a claim, and until when.
--
-- Recovery previously could not tell a job abandoned by a dead worker from one a
-- live worker is still running: the only signal was a process starting up, and
-- "I just started" says nothing about anybody else. So recovery had to assume
-- every running job was abandoned, which made an API restart requeue work the
-- worker was still doing, and made a second worker replica unsafe by
-- construction.
--
-- A lease turns that into a fact the database holds. A worker renews while it
-- runs, so an expired lease means nobody is renewing it — the job is genuinely
-- abandoned, whoever asks and whenever. That makes reclaim safe to run from any
-- process at any time, which is what several replicas need.
--
-- Both columns are nullable, and NULL reads as expired: rows claimed by a worker
-- from before this migration have nobody renewing them either.
ALTER TABLE job ADD COLUMN IF NOT EXISTS claimed_by TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS lease_until BIGINT;

-- Reclaim scans running jobs by expiry, and running jobs are a small fraction of
-- the table, so the index is partial.
CREATE INDEX IF NOT EXISTS job_lease ON job (lease_until) WHERE status = 'running';
