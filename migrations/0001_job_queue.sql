-- Job queue: jobs, their input bodies, and their results.
--
-- Timestamps are BIGINT Unix seconds rather than TIMESTAMPTZ. That is deliberate:
-- the Job JSON exposes seconds and the front-end does new Date(sec*1000), so
-- storing seconds keeps one representation end to end.

CREATE TABLE IF NOT EXISTS job (
  id          TEXT PRIMARY KEY,
  kind        TEXT   NOT NULL,
  snapshot    TEXT   NOT NULL,
  selection   TEXT   NOT NULL DEFAULT '',
  status      TEXT   NOT NULL,
  error       TEXT,
  n_variants  BIGINT,
  client_ip   TEXT   NOT NULL DEFAULT '',
  session_id  TEXT   NOT NULL DEFAULT '',
  label       TEXT   NOT NULL DEFAULT '',
  created_at  BIGINT NOT NULL,
  started_at  BIGINT,
  finished_at BIGINT
);

CREATE INDEX IF NOT EXISTS job_status   ON job (status, created_at);
CREATE INDEX IF NOT EXISTS job_finished ON job (finished_at);
CREATE INDEX IF NOT EXISTS job_session  ON job (session_id, created_at);

-- The fair scheduler counts running jobs per client_ip on every claim. A partial
-- index keeps that a small scan of just the running set rather than the whole
-- table, which is what the SQLite version effectively did at its scale.
CREATE INDEX IF NOT EXISTS job_running_ip ON job (client_ip) WHERE status = 'running';

-- Claiming orders by (running-count, created_at, id) over queued rows only.
CREATE INDEX IF NOT EXISTS job_queued_order
  ON job (created_at, id) WHERE status = 'queued';

CREATE TABLE IF NOT EXISTS job_input (
  job_id TEXT PRIMARY KEY REFERENCES job (id) ON DELETE CASCADE,
  body   BYTEA NOT NULL
);

CREATE TABLE IF NOT EXISTS job_result (
  job_id TEXT PRIMARY KEY REFERENCES job (id) ON DELETE CASCADE,
  json   TEXT NOT NULL
);
