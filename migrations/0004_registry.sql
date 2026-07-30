-- Configured source registries.
--
-- A registry is a static registry.toml served over HTTPS listing source and
-- snapshot *configs* (not data). It is the same file `varhub registry` reads, so
-- a registry published for the CLI works here unchanged.
--
-- Only the location is stored. The catalog is fetched live rather than mirrored:
-- a registry gains sources over time, and a stale local copy would quietly hide
-- them.

CREATE TABLE IF NOT EXISTS registry (
  id         TEXT   PRIMARY KEY,
  name       TEXT   NOT NULL,
  url        TEXT   NOT NULL,
  builtin    BOOLEAN NOT NULL DEFAULT false, -- seeded default, not user-added
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
