-- Where a reference's durable copy lives.
--
-- A tool binds the FASTA's directory into its container, so the file must exist
-- on the worker's own disk at run time — that copy is not optional and is not
-- what this names. This names where the durable copy is kept, so a worker
-- starting with an empty disk can restore it instead of re-fetching from the
-- origin, which for GRCh38 is most of a gigabyte over someone else's FTP.
--
-- The same durable/working split tool data already uses (see source_settings'
-- cache_setup). Empty means the local copy is the only copy.
ALTER TABLE reference ADD COLUMN IF NOT EXISTS storage_id TEXT NOT NULL DEFAULT '';
ALTER TABLE reference ADD COLUMN IF NOT EXISTS durable_uri TEXT NOT NULL DEFAULT '';
