-- The first way in.
--
-- An installation with no accounts cannot be administered, and an administrator
-- cannot be created without administering. Something has to break that circle.
-- The service mints one bootstrap token at startup when no administrator exists
-- and logs it once; it is used to create the first real account and nothing else.
--
-- Deliberately its own table rather than a row in api_token: that table's tokens
-- belong to an account, which is exactly what this one cannot have. Keeping it
-- separate also means the bootstrap has a lifecycle of its own — it is consumed,
-- not revoked, and the row records when.
CREATE TABLE IF NOT EXISTS bootstrap_token (
  id         TEXT PRIMARY KEY,
  prefix     TEXT NOT NULL,
  hash       TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  -- Set when the token was used to create the first administrator. A consumed
  -- token never authenticates again, so a leaked startup log is not a way in.
  consumed_at BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS bootstrap_token_prefix ON bootstrap_token (prefix);
