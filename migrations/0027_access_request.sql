-- Somebody who authenticated but has no account here.
--
-- Registration is invite-only: CILogon federates thousands of institutions, so
-- it vouching for a person is not this deployment vouching for them. That left
-- everyone outside an allow-listed domain at a dead end — a correct refusal
-- with nothing to do about it but find an administrator's address.
--
-- A row here is that dead end turned into a queue. The person is already known:
-- the provider verified their address, so nothing on this row is self-asserted
-- and there is nothing for them to fill in. Approving it creates the account
-- their next sign-in claims.
CREATE TABLE IF NOT EXISTS access_request (
  id       TEXT PRIMARY KEY,
  -- The address the provider reported, never one that was typed. Approval
  -- creates an account with it, so a self-asserted value here would be a way to
  -- be granted somebody else's identity.
  email    TEXT NOT NULL,
  name     TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL,
  -- The provider's subject claim: stable across an address change, and what
  -- makes a second sign-in find the same request rather than making a new one.
  subject  TEXT NOT NULL,

  -- pending | approved | declined. A decided row is kept rather than deleted:
  -- a declined request that vanishes is one the same person raises again next
  -- week, with nothing to say it was already considered.
  status     TEXT NOT NULL DEFAULT 'pending',
  decided_by TEXT REFERENCES app_user (id) ON DELETE SET NULL,
  decided_at BIGINT NOT NULL DEFAULT 0,

  created_at BIGINT NOT NULL,
  -- Bumped every time they try again, so an administrator can tell somebody
  -- still trying from somebody who gave up in March.
  last_seen_at BIGINT NOT NULL,

  -- One request per identity. A second sign-in updates the row it already has.
  UNIQUE (provider, subject)
);

-- The review screen reads pending first, oldest first.
CREATE INDEX IF NOT EXISTS access_request_status
  ON access_request (status, created_at);
