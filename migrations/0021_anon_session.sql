-- A server-issued session for a visitor who has not signed in.
--
-- Anonymous access used to rest on vh_history: a random id the browser made up
-- and sent in a header. That is fine for "which results are mine" in a UI, but
-- it is self-asserted, so it can gate nothing — anything can send any value.
-- The consequence was that a bare curl with no credential at all was
-- indistinguishable from an anonymous visitor, and could submit jobs against
-- the published API.
--
-- So the server issues one instead, handed out with the app shell. A caller now
-- presents a session or a token; presenting neither is not a visitor, it is an
-- unauthenticated request.
--
-- Deliberately NOT a row in user_session with a null user. That table's rows
-- mean "this is a signed-in person", enforced by a NOT NULL reference to
-- app_user, and relaxing it to carry anonymous sessions too would put one
-- resolve-a-session query one bug away from treating a visitor as an account.
CREATE TABLE IF NOT EXISTS anon_session (
  id         TEXT PRIMARY KEY,
  created_at BIGINT NOT NULL,
  -- Refreshed as the visitor uses the site, so expiry follows activity rather
  -- than first contact — a job history that vanished mid-session because the
  -- window opened a day ago would be the wrong behaviour.
  seen_at    BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS anon_session_seen ON anon_session (seen_at);
