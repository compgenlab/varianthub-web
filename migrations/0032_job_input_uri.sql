-- A job's input, by reference rather than by value.
--
-- job_input.body is a BYTEA holding the whole submission. That was right while
-- an input was a few thousand loci; it is not right for a VCF. Chromosome 22 of
-- a WGS cohort is 26 MB compressed and 2.6M variants, and the bytes are read
-- into memory by the API to accept them, written to Postgres, read back whole by
-- the worker, and copied again as a string by the cache's parser — several
-- resident copies across two processes of something only ever read start to end.
-- The upload cap cannot rise while that is true, and it has to rise.
--
-- The input now lives in job storage (a bucket, or a shared directory) and this
-- table holds where. Postgres stops being a file store, the API streams an
-- upload straight through, and the worker stages it to the local file the engine
-- wanted all along.
ALTER TABLE job_input ADD COLUMN IF NOT EXISTS uri TEXT;

-- body becomes optional, because a job whose input is a URI has none.
--
-- Not dropped. Existing jobs have bodies and no URI, and they must keep working
-- until they age out through the ordinary TTL sweep — a migration that discarded
-- them would fail every queued job at the moment of deploy. Readers take
-- whichever is present; new writes set exactly one.
ALTER TABLE job_input ALTER COLUMN body DROP NOT NULL;

-- Exactly one of the two, never both and never neither.
--
-- Written as a constraint rather than trusted to the code because "neither" is a
-- job that can be claimed and then cannot run, and "both" is two inputs with no
-- rule about which wins. Both are silent: the first fails at claim time with a
-- confusing error, the second annotates whichever the reader happens to check
-- first.
ALTER TABLE job_input DROP CONSTRAINT IF EXISTS job_input_one_source;
ALTER TABLE job_input ADD CONSTRAINT job_input_one_source
  CHECK ((body IS NULL) <> (uri IS NULL));
