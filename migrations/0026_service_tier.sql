-- What an account is allowed to ask of the service.
--
-- Separate from role, which is permission: role says what you may administer,
-- this says how much of the pool you may occupy. Conflating them would mean
-- raising someone's limits by making them an administrator.
--
-- Three tiers rather than a number per account. The numbers belong to the
-- deployment and change together — an operator who buys more workers raises
-- what everyone gets, and doing that across a column of per-account integers is
-- a migration every time. A tier is a name an administrator assigns; what the
-- name means lives in site_setting beside the other things they can change
-- without a redeploy.
ALTER TABLE app_user
  ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'standard';

-- The concurrency limit that applied when the job was accepted, denormalized
-- onto the row the way weight already is.
--
-- On the job rather than read from app_user at dispatch, for three reasons. The
-- claim query stays a single statement over one table, which is what lets it
-- run under FOR UPDATE SKIP LOCKED without joining the identity schema. A job
-- keeps the terms it was admitted under, so raising a tier does not retroac-
-- tively change what is already queued. And an anonymous job has no account to
-- read a limit from, but still needs one.
--
-- 0 means "no limit of its own": the dispatcher falls back to the deployment's
-- per-IP cap, which is the pre-tier behaviour and what anonymous work gets.
ALTER TABLE job
  ADD COLUMN IF NOT EXISTS max_concurrent INTEGER NOT NULL DEFAULT 0;

-- How the job was submitted: 'web' for a browser session, 'api' for a personal
-- access token, '' for anything recorded before this column existed.
--
-- Empty rather than a guess. The two are genuinely different populations — one
-- is a person clicking, the other a script in a loop — and reporting historical
-- rows as either would put a number on a distinction that was never recorded.
ALTER TABLE job
  ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT '';

-- The usage summary counts jobs per account over a window, and per origin.
CREATE INDEX IF NOT EXISTS job_user_created ON job (user_id, created_at)
  WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS job_created ON job (created_at);
