-- Deployment settings an administrator can change without a redeploy.
--
-- config.toml states the defaults; a row here overrides one. Absent means "use
-- the default", which is one state rather than two — there is no row meaning
-- "same as the default", so reverting is a delete and the file stays the single
-- description of how this installation was set up.
--
-- Values are text and parsed per key. The alternative, a typed column per
-- setting, makes every new setting a migration; this table is small and read
-- through one accessor that knows what each key means.
CREATE TABLE IF NOT EXISTS site_setting (
  key        TEXT   PRIMARY KEY,
  value      TEXT   NOT NULL,
  updated_at BIGINT NOT NULL
);
