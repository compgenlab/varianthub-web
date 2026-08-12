-- Three visibility levels.
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
--
-- Only sources carry a level. A snapshot's is *derived* — the most restrictive of
-- everything it pins — and deliberately not stored: a snapshot is a claim about
-- which annotations a result carries, so it can never be offered more widely than
-- the sources behind it, and a second place to say so could only ever disagree
-- with the first. See catalog.Snapshot.EffectiveVisibility.
ALTER TABLE source DROP CONSTRAINT IF EXISTS source_visibility;

UPDATE source SET visibility = 'restricted' WHERE visibility = 'private';

ALTER TABLE source
  ADD CONSTRAINT source_visibility
  CHECK (visibility IN ('public', 'signed_in', 'restricted'));
