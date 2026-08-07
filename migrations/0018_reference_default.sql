-- The reference genome an ad-hoc snapshot should pin, per assembly.
--
-- References are ordinary sources (type = "reference"), pinned by a snapshot so
-- a run stays reproducible when a newer patch release appears. A curated
-- snapshot names the one it wants. An ad-hoc snapshot is assembled per job from
-- whatever the caller selected, and has nobody to ask — so one reference per
-- assembly is marked as the one to reach for.
--
-- This is a selection made when the ad-hoc snapshot is created, not an
-- indirection resolved at run time: the snapshot still pins the chosen source,
-- so re-running it later cannot silently drift onto a newer genome.
--
-- The partial unique index enforces "at most one default per assembly" in the
-- database rather than in a handler, because two would make the ad-hoc choice
-- arbitrary and the arbitrariness would be invisible.
ALTER TABLE source ADD COLUMN IF NOT EXISTS is_default_reference BOOLEAN NOT NULL DEFAULT FALSE;

CREATE UNIQUE INDEX IF NOT EXISTS source_one_default_reference
  ON source (build) WHERE is_default_reference;
