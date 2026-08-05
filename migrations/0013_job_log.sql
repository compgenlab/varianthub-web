-- What a job's run actually printed.
--
-- The one-line error stored on the job answers "what went wrong"; this answers
-- "what happened", which is a different question and the one asked when the
-- first answer is not enough. It reached the container log and nowhere else, so
-- reading it meant shell access to the worker — not something a catalog admin
-- has, and gone entirely once the container is replaced.
--
-- Its own table, like job_input and job_result: the row is large and read only
-- when someone opens a job, so it stays out of every listing query.
CREATE TABLE IF NOT EXISTS job_log (
  job_id TEXT PRIMARY KEY REFERENCES job (id) ON DELETE CASCADE,
  output TEXT NOT NULL
);
