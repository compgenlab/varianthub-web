-- Accounts, teams, and grants on private sources.
--
-- Until now a caller was either "holds the shared token" or "asserted a session
-- id in a header". The second was self-asserted: nothing verified it, so anyone
-- who learned a session id could read that session's results. Identity moves to
-- rows the server issues.

CREATE TABLE IF NOT EXISTS app_user (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  -- 'admin' administers the catalog; 'member' annotates. Deliberately two: a
  -- richer scheme can be added when something needs it, and guessing now would
  -- mean enforcing distinctions nobody has asked for.
  role          TEXT NOT NULL DEFAULT 'member',
  -- Empty for an account that authenticates elsewhere (SSO), which is why this
  -- is not NOT NULL-with-no-default: the column has to admit "not applicable".
  password_hash TEXT NOT NULL DEFAULT '',
  disabled      BOOLEAN NOT NULL DEFAULT FALSE,
  created_at    BIGINT NOT NULL,
  updated_at    BIGINT NOT NULL
);

-- Case-insensitive: an address differing only in case is the same person, and
-- letting both exist would silently split one account's grants across two.
CREATE UNIQUE INDEX IF NOT EXISTS app_user_email ON app_user (lower(email));

CREATE TABLE IF NOT EXISTS team (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  created_at BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS team_name ON team (lower(name));

CREATE TABLE IF NOT EXISTS team_member (
  team_id TEXT NOT NULL REFERENCES team (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
  role    TEXT NOT NULL DEFAULT 'member',  -- 'owner' may change membership
  PRIMARY KEY (team_id, user_id)
);
CREATE INDEX IF NOT EXISTS team_member_by_user ON team_member (user_id);

-- Personal API tokens. Only the hash is stored: a token is shown once at
-- creation and cannot be recovered, so a database leak does not yield working
-- credentials. The prefix is kept in clear to identify which token a caller
-- presented without reversing the hash, and to make a leaked token greppable.
CREATE TABLE IF NOT EXISTS api_token (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
  name         TEXT NOT NULL DEFAULT '',
  prefix       TEXT NOT NULL,
  hash         TEXT NOT NULL,
  created_at   BIGINT NOT NULL,
  last_used_at BIGINT NOT NULL DEFAULT 0,
  revoked_at   BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS api_token_prefix ON api_token (prefix);
CREATE INDEX IF NOT EXISTS api_token_by_user ON api_token (user_id);

CREATE TABLE IF NOT EXISTS user_session (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS user_session_by_user ON user_session (user_id);
CREATE INDEX IF NOT EXISTS user_session_expiry ON user_session (expires_at);

-- Which teams may see a private source. A public source needs no row; absence
-- of a grant on a private source means invisible, so the default is closed.
CREATE TABLE IF NOT EXISTS source_grant (
  source_id  TEXT NOT NULL REFERENCES source (id) ON DELETE CASCADE,
  team_id    TEXT NOT NULL REFERENCES team (id) ON DELETE CASCADE,
  granted_by TEXT NOT NULL DEFAULT '',
  granted_at BIGINT NOT NULL,
  PRIMARY KEY (source_id, team_id)
);
CREATE INDEX IF NOT EXISTS source_grant_by_team ON source_grant (team_id);

-- Jobs gain a real owner. The existing session column stays: it still scopes an
-- anonymous caller's own history, and back-filling it with a user is not
-- possible for jobs submitted before accounts existed.
ALTER TABLE job ADD COLUMN IF NOT EXISTS user_id TEXT;
CREATE INDEX IF NOT EXISTS job_by_user ON job (user_id, created_at DESC);
