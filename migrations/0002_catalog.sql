-- The annotation catalog: sources, snapshots, and which sources a snapshot pins.
--
-- Each manifest is stored as its **TOML text**, verbatim. That is deliberate:
--
--   * varhub already has a well-tested TOML parser for these fragments, and
--     round-tripping them through relational columns and back would be a second
--     implementation of that schema, free to drift from the first.
--   * the admin UI edits TOML directly (see the design handoff), so storing what
--     the user wrote means what they see is what runs.
--   * source fragments carry a large, evolving field set — build recipes, tool
--     steps, per-chromosome URL templates. Columns would need a migration per
--     CLI feature.
--
-- The columns alongside toml_text are a projection for querying and display
-- (list, filter, show a badge). They are derived from the TOML at write time,
-- never the other way round: toml_text is the source of truth.

CREATE TABLE IF NOT EXISTS source (
  id           TEXT PRIMARY KEY,                  -- slug used in URLs, e.g. "clinvar"
  name         TEXT   NOT NULL,                   -- varhub source name
  version      TEXT   NOT NULL,
  title        TEXT   NOT NULL DEFAULT '',
  detail       TEXT   NOT NULL DEFAULT '',        -- one-line description for the picker
  kind         TEXT   NOT NULL,                   -- builtin|vcf|bed|gtf|tab|genelist|tool
  build        TEXT   NOT NULL DEFAULT '',        -- assembly, '' = build-independent
  visibility   TEXT   NOT NULL DEFAULT 'public',  -- public|private
  index_status TEXT   NOT NULL DEFAULT 'indexed', -- indexed|building|error
  origin       TEXT   NOT NULL DEFAULT '',        -- provenance, e.g. "registry: ncbi-clinvar"
  toml_text    TEXT   NOT NULL,                   -- the [[sources]] fragment, verbatim
  created_at   BIGINT NOT NULL,
  updated_at   BIGINT NOT NULL,
  CONSTRAINT source_name_version UNIQUE (name, version),
  CONSTRAINT source_visibility CHECK (visibility IN ('public', 'private')),
  CONSTRAINT source_index_status CHECK (index_status IN ('indexed', 'building', 'error'))
);

CREATE INDEX IF NOT EXISTS source_kind       ON source (kind);
CREATE INDEX IF NOT EXISTS source_visibility ON source (visibility);

CREATE TABLE IF NOT EXISTS snapshot (
  id           TEXT   PRIMARY KEY,                -- also the varhub snapshot name
  title        TEXT   NOT NULL DEFAULT '',
  description  TEXT   NOT NULL DEFAULT '',
  build        TEXT   NOT NULL,                   -- assembly, e.g. GRCh38
  state        TEXT   NOT NULL DEFAULT 'draft',   -- draft|published
  defaults     TEXT[] NOT NULL DEFAULT '{}',      -- default annotation names
  tags         TEXT[] NOT NULL DEFAULT '{}',
  published_at BIGINT,
  created_at   BIGINT NOT NULL,
  updated_at   BIGINT NOT NULL,
  CONSTRAINT snapshot_state CHECK (state IN ('draft', 'published'))
);

CREATE INDEX IF NOT EXISTS snapshot_state ON snapshot (state, build);

CREATE TABLE IF NOT EXISTS snapshot_source (
  snapshot_id TEXT NOT NULL REFERENCES snapshot (id) ON DELETE CASCADE,
  -- RESTRICT, not CASCADE: deleting a source that a snapshot pins would silently
  -- change what that snapshot means, and published snapshots are meant to be
  -- reproducible. Detach it from the snapshot first.
  source_id   TEXT NOT NULL REFERENCES source (id) ON DELETE RESTRICT,
  position    INT  NOT NULL DEFAULT 0,            -- ordering within the snapshot
  PRIMARY KEY (snapshot_id, source_id)
);

CREATE INDEX IF NOT EXISTS snapshot_source_by_source ON snapshot_source (source_id);
