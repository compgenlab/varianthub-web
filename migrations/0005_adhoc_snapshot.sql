-- Allow an "adhoc" snapshot state.
--
-- Choosing individual sources instead of a snapshot still has to produce a
-- snapshot: that is what the engine annotates against, what gets materialized,
-- and what makes a job reproducible after the fact. So an ad-hoc selection is
-- persisted as a real snapshot, just one that is not offered in the picker.
--
-- It is a distinct state rather than a draft because the two mean different
-- things: a draft is something an admin is still editing and may publish, while
-- an ad-hoc row is machine-generated from one submission and should never appear
-- in the admin list.

ALTER TABLE snapshot DROP CONSTRAINT IF EXISTS snapshot_state;
ALTER TABLE snapshot ADD CONSTRAINT snapshot_state
  CHECK (state IN ('draft', 'published', 'adhoc'));
