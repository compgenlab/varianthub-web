-- External sign-in identities (CILogon today; the shape admits others).
--
-- Separate from app_user because the relationship is one-to-many and because
-- the join key is the provider's, not ours: an institution's OIDC `sub` is
-- stable across email changes, so it — and not the address — is what identifies
-- a returning person.
--
-- An account may hold identities *and* a password, or either alone. An account
-- with no password is what internal/identity reports as SSO: there is nothing
-- stored here to change or to leak.
CREATE TABLE IF NOT EXISTS user_identity (
  id       TEXT PRIMARY KEY,
  user_id  TEXT NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
  -- Short stable key: 'cilogon'. Not the issuer URL, which can be re-pointed.
  provider TEXT NOT NULL,
  -- The provider's subject claim. Opaque; never parsed.
  subject  TEXT NOT NULL,
  -- The address the provider reported when the link was made. Kept for display
  -- and for audit — the account's own email stays authoritative, so a provider
  -- changing what it reports cannot silently move the account.
  email      TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  last_seen_at BIGINT NOT NULL DEFAULT 0,
  UNIQUE (provider, subject)
);
CREATE INDEX IF NOT EXISTS user_identity_by_user ON user_identity (user_id);
