-- Reference genomes, as catalog data rather than deployment configuration.
--
-- They were a config map of assembly -> path, which meant a reference could only
-- be added by editing a file on the host and restarting, and only to a path that
-- already existed there. That is the wrong shape: which references exist is a
-- fact about the installation, like its sources and snapshots, and belongs where
-- an administrator can see and change it.
--
-- Keyed by assembly, because that is what a lookup asks: a tool step rendering
-- {ref} wants "the FASTA for GRCh38", and a second FASTA for the same assembly
-- would make that question ambiguous. Assembly names are compared exactly and
-- deliberately not normalized, for the same reason they are everywhere else here
-- — "GRCh38" and "hg38" are different keys, because a false match annotates
-- against the wrong coordinates and says nothing.
CREATE TABLE IF NOT EXISTS reference (
  assembly    TEXT PRIMARY KEY,
  -- Where the bytes come from: an https:// or s3:// URI. Kept after
  -- provisioning so the same reference can be re-fetched onto another machine,
  -- and so it is possible to see what a file actually is.
  uri         TEXT NOT NULL,
  checksum    TEXT NOT NULL DEFAULT '',
  -- Where it landed on the worker's filesystem. Empty until provisioned.
  --
  -- A path and not a locator: a tool step binds the FASTA's directory into a
  -- container, so this one cannot live in an object store however much the
  -- source data does.
  path        TEXT NOT NULL DEFAULT '',
  size_bytes  BIGINT NOT NULL DEFAULT 0,
  -- installing | ready | failed, mirroring source_state so the UI can render
  -- both the same way.
  state       TEXT NOT NULL DEFAULT 'installing',
  error       TEXT NOT NULL DEFAULT '',
  created_at  BIGINT NOT NULL,
  updated_at  BIGINT NOT NULL
);
