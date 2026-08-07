-- How long a personal API token stays valid.
--
-- Tokens were issued without an end, so a leaked one worked until somebody
-- noticed and revoked it. A chosen lifetime makes the common case — a token
-- minted to try something out — stop mattering on its own.
--
-- 0 means no expiry, which is what every token issued before this column
-- existed gets. Retrofitting a deadline onto credentials already in use would
-- break whatever holds them at a moment nobody chose; they stay valid until
-- revoked, and new ones must state a lifetime.
ALTER TABLE api_token ADD COLUMN IF NOT EXISTS expires_at BIGINT NOT NULL DEFAULT 0;

-- Finding what is about to lapse, and sweeping what has.
CREATE INDEX IF NOT EXISTS api_token_expiry ON api_token (expires_at)
  WHERE expires_at > 0;
