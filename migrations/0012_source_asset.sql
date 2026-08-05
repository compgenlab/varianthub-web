-- Helper files a source's build recipe or tool steps need.
--
-- A recipe like REVEL's converts its inputs with a Python script that ships
-- beside the manifest in the registry. The manifest names it; without the file
-- itself the recipe fails at the first step, so the two have to travel together.
--
-- Stored as rows rather than on disk because the catalog is the source of truth
-- and the worker materializes a config per job — an asset on one machine's
-- filesystem would be invisible to every other worker.
CREATE TABLE IF NOT EXISTS source_asset (
  source_id  TEXT   NOT NULL REFERENCES source (id) ON DELETE CASCADE,
  -- The name exactly as the manifest lists it, since that is what the recipe
  -- refers to. May contain a subdirectory.
  name       TEXT   NOT NULL,
  content    BYTEA  NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (source_id, name)
);
