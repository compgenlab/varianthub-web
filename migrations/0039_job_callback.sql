-- Telling a caller's own server that a job has finished.
--
-- Polling is what a laptop does, because nothing can reach it. A service that
-- submits work and has an address of its own should not have to ask every few
-- seconds whether the thing it started is done yet.
--
-- callback_at is the interesting column, and it is not a timestamp for the sake
-- of one. A fan-out job finishes when its last chunk does, and twenty-six
-- workers may be closing chunks in the same instant — each of them able to
-- observe that the job is now terminal. Claiming this column with a conditional
-- UPDATE is what makes exactly one of them send: the winner takes the row, the
-- rest see it already taken and do nothing. Without it a split job would notify
-- once per chunk that happened to look last.
--
-- Deliberately no secret and no signature. The job id is a 128-bit value known
-- only to the submitter and this service, so it already carries as much proof of
-- origin as a shared key would for a message that says no more than "job X ended
-- this way" — and the payload is two fields precisely so there is nothing in it
-- worth forging. A receiver that needs certainty asks GET /jobs/{id} with its
-- own credential.
ALTER TABLE job ADD COLUMN IF NOT EXISTS callback_url TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS callback_at  BIGINT;

-- Finding the jobs still owed a notification, without walking the ones that
-- never asked for one.
CREATE INDEX IF NOT EXISTS job_callback_pending
  ON job (id) WHERE callback_url IS NOT NULL AND callback_at IS NULL;
