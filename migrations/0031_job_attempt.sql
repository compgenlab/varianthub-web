-- One row per time a job was handed to a worker.
--
-- job.attempts already counts them, and the job's log narrates them. Those two
-- answer "did this job get retried" and "what was it doing", which covers the
-- ordinary case. What they cannot answer is anything aggregate, because a
-- counter has no dimensions and a log is prose:
--
--   * which worker lost the attempts — a counter of 3 does not say whether one
--     pod ate all three or three pods each lost one, and those are different
--     problems with different fixes;
--   * how long an attempt survived before it died — an OOM kill on a big job
--     looks like a long attempt that never reported, and a crash on startup
--     looks like a short one, and the counter shows the same number for both;
--   * what a worker's abandonment rate is over a window, which is the question
--     actually being asked when a container keeps restarting.
--
-- So this is the more complete form of a record already being kept in two
-- lossy ones. It costs a row per attempt — the same order as job_log, and far
-- less than job_variant — and it is written on paths that already have a
-- transaction open.
CREATE TABLE IF NOT EXISTS job_attempt (
  job_id     TEXT   NOT NULL REFERENCES job (id) ON DELETE CASCADE,
  -- Matches job.attempts as it stood when this attempt was claimed, so the two
  -- records can be reconciled: the highest attempt here should equal the
  -- counter there.
  attempt    INTEGER NOT NULL,
  -- The process that claimed it. Recorded at claim time because the reclaim
  -- clears job.claimed_by — after an abandonment that identity exists nowhere
  -- else, which is the gap this table is mainly here to close.
  worker     TEXT   NOT NULL,
  started_at BIGINT NOT NULL,

  -- NULL while the attempt is in flight. An attempt whose row still has NULL
  -- here long after started_at is one whose worker never came back, which is
  -- the abandonment before the sweep has noticed it.
  ended_at   BIGINT,
  -- done | error | cancelled | abandoned. NULL while running.
  --
  -- "abandoned" is deliberately a peer of the others rather than a flavour of
  -- error: the job did not fail, the process running it disappeared, and
  -- folding the two together is what hides a deployment losing capacity inside
  -- an ordinary error rate.
  outcome    TEXT,
  -- What this attempt reported, when it reported anything. Kept per attempt
  -- rather than only on the job, because a job that fails differently each time
  -- is a different diagnosis from one that fails the same way three times, and
  -- job.error only holds the last.
  error      TEXT,

  PRIMARY KEY (job_id, attempt)
);

-- "Is one worker losing all of them?" — the query this table exists to make
-- possible, so it gets the index.
CREATE INDEX IF NOT EXISTS job_attempt_worker ON job_attempt (worker, started_at);

-- The open attempts, for finding leases nobody is renewing and for closing an
-- attempt when its job finishes. Partial, because the in-flight set is tiny
-- next to the history and this index should stay that size.
CREATE INDEX IF NOT EXISTS job_attempt_open ON job_attempt (job_id) WHERE outcome IS NULL;
