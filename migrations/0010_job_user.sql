-- Jobs gain a real owner.
--
-- Separate from the auth schema because it belongs to the queue: the queue is
-- usable — and tested — without accounts, and making its table depend on the
-- account tables would tie the two together for one nullable column.
--
-- The existing session column stays. It still scopes an anonymous caller's own
-- history, and back-filling it with a user is not possible for jobs submitted
-- before accounts existed. Where both are present the user is authoritative: a
-- session id is asserted by the client, while this is written from a credential
-- the server verified.
ALTER TABLE job ADD COLUMN IF NOT EXISTS user_id TEXT;
CREATE INDEX IF NOT EXISTS job_by_user ON job (user_id, created_at DESC);
