-- Storage locations and the files downloaded into them.
--
-- A source's manifest says where its data comes from; a storage location says
-- where the copy lives. They are separate because the same source can be
-- provisioned into different storage on different deployments, and because the
-- files are large enough that where they land is an operational decision, not a
-- property of the source.

CREATE TABLE IF NOT EXISTS storage_location (
  id         TEXT   PRIMARY KEY,
  name       TEXT   NOT NULL,
  kind       TEXT   NOT NULL,                  -- path | s3
  uri        TEXT   NOT NULL,                  -- filesystem path, or s3://bucket/prefix
  -- Locations declared in the service config are managed by the deployment and
  -- cannot be edited or removed through the API; the row exists so downloads can
  -- reference them like any other.
  from_config BOOLEAN NOT NULL DEFAULT false,
  is_default  BOOLEAN NOT NULL DEFAULT false,
  created_at  BIGINT NOT NULL,
  updated_at  BIGINT NOT NULL,
  CONSTRAINT storage_kind CHECK (kind IN ('path', 's3'))
);

-- Files downloaded for a source, recorded by the worker after a successful
-- download. Scoped by location: the same source provisioned into two locations
-- has two sets of rows, and removing a location's copy does not imply the other
-- is gone.
CREATE TABLE IF NOT EXISTS source_file (
  source_id   TEXT   NOT NULL REFERENCES source (id) ON DELETE CASCADE,
  storage_id  TEXT   NOT NULL REFERENCES storage_location (id) ON DELETE CASCADE,
  path        TEXT   NOT NULL,                 -- relative to the location's root
  size_bytes  BIGINT NOT NULL,
  modified_at BIGINT NOT NULL,
  recorded_at BIGINT NOT NULL,
  PRIMARY KEY (source_id, storage_id, path)
);

CREATE INDEX IF NOT EXISTS source_file_by_storage ON source_file (storage_id);
