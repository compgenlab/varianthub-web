-- Whether a source is actually usable yet.
--
-- Registering a source and being able to annotate with it are different things:
-- a tool needs its image pulled and its setup run, a build source needs its
-- recipe to have produced an output. Until then the source exists and every
-- annotation using it fails.
--
-- Stored rather than derived from the download jobs. Terminal jobs are garbage
-- collected — job_ttl defaults to 24 hours — so a state read from them would be
-- correct on the day it was installed and wrong forever after. A tool records no
-- files either: its image and data go to the worker's local data_dir rather than
-- to a storage location, so "has files" answers no for it permanently.
CREATE TABLE IF NOT EXISTS source_state (
  source_id  TEXT PRIMARY KEY REFERENCES source (id) ON DELETE CASCADE,
  -- installing | ready | failed
  state      TEXT   NOT NULL,
  -- The last failure, kept so a source that cannot be provisioned says why
  -- where someone is looking at it rather than only in a job that will be
  -- collected.
  error      TEXT   NOT NULL DEFAULT '',
  updated_at BIGINT NOT NULL
);
