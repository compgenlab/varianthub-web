-- Deployment-local settings for a source.
--
-- Kept out of the source row because toml_text is the registry's and these are
-- ours: re-fetching a manifest replaces what the author wrote and must not
-- silently discard what this catalog decided about it. A separate row also
-- makes "settings exist" answerable without parsing anything.
CREATE TABLE IF NOT EXISTS source_settings (
  source_id TEXT PRIMARY KEY REFERENCES source (id) ON DELETE CASCADE,
  -- Renames this source's output fields for this deployment. "-" means no
  -- prefix at all, which an empty string cannot express: empty has to mean
  -- "not set" so the manifest's own default still applies.
  annotation_prefix TEXT NOT NULL DEFAULT '',
  -- Publish this tool's setup output so a machine with none can restore it.
  -- Only meaningful for a tool source provisioned to an object store.
  cache_setup BOOLEAN NOT NULL DEFAULT false,
  updated_at BIGINT NOT NULL
);
