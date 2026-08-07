-- Move helper-file bytes out of Postgres and into the storage a source's data
-- already uses.
--
-- 0012 put them in a BYTEA because the catalog is the source of truth and an
-- asset on one worker's filesystem is invisible to every other one. That
-- reasoning still holds; the database was just the wrong place to satisfy it.
-- These are scripts a build step executes — the same class of thing as the data
-- and tool caches, which live in object storage — and Postgres is where the
-- catalog's *index* belongs, not its payload.
--
-- So the row stays as the index and the bytes move. The object is named by the
-- SHA-256 of its own content, which buys two things: a script shared by two
-- sources is stored once, and a fetch can verify what it got is what was asked
-- for, since the name is a claim about the content that the content can settle.
ALTER TABLE source_asset ADD COLUMN IF NOT EXISTS sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE source_asset ADD COLUMN IF NOT EXISTS size_bytes BIGINT NOT NULL DEFAULT 0;

-- Nullable, not dropped. Existing rows still hold the only copy of their bytes
-- until the backfill has uploaded them, and an installation with no storage
-- location configured keeps working by leaving them here. A row has its content
-- inline or a digest, never neither.
ALTER TABLE source_asset ALTER COLUMN content DROP NOT NULL;

-- Finding every object a source still references, so a future sweep can tell a
-- live asset from an orphan left by a re-registration.
CREATE INDEX IF NOT EXISTS source_asset_sha256_idx ON source_asset (sha256)
  WHERE sha256 <> '';
