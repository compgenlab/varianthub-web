-- Three visibility levels, and snapshots get their own.
--
-- The old model had two values and conflated two different questions in one of
-- them: "private" meant "an administrator, or a team this was explicitly granted
-- to", which is the right answer for data under an agreement and much too strong
-- for the far more common case of "not for anonymous visitors". There was no way
-- to express the second, so anything wanting it had to be granted to a team per
-- source — administration that grows with the catalog, to say something that is
-- really a property of the deployment.
--
--   public      anyone who can reach the server, including anonymous visitors
--   signed_in   any account; no grant needed
--   restricted  membership of a team the source is granted to
--
-- Renaming private → restricted rather than keeping the word: what it means is
-- unchanged, but next to "signed_in" the old name reads as "the other one", and
-- an administrator choosing between them has to be told which is stronger.
ALTER TABLE source DROP CONSTRAINT IF EXISTS source_visibility;

UPDATE source SET visibility = 'restricted' WHERE visibility = 'private';

ALTER TABLE source
  ADD CONSTRAINT source_visibility
  CHECK (visibility IN ('public', 'signed_in', 'restricted'));

-- Snapshots had no visibility of their own: it was derived from what they pinned,
-- and a snapshot was hidden entirely unless every pinned source was visible. That
-- derivation stays — a snapshot is a claim about which annotations a result
-- carries, and showing one with a source quietly removed answers a different
-- question than the one asked. What is added is the ability to restrict a
-- snapshot *further* than its sources, which the derivation alone cannot express:
-- a bundle assembled for one group out of sources that are individually public.
--
-- Effective visibility is therefore the most restrictive of the snapshot's own
-- level and every source it pins. The stored value can only ever narrow.
--
-- Defaulting to public preserves today's behaviour exactly: before this column
-- existed the answer came entirely from the sources, and public is the identity
-- element of "most restrictive wins".
ALTER TABLE snapshot
  ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'public';

ALTER TABLE snapshot DROP CONSTRAINT IF EXISTS snapshot_visibility;

ALTER TABLE snapshot
  ADD CONSTRAINT snapshot_visibility
  CHECK (visibility IN ('public', 'signed_in', 'restricted'));
