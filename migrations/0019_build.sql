-- Genome builds the installation offers.
--
-- The annotation form used a hardcoded list of four strings, so there was no way
-- to add one and no way to filter sources by the one selected — the picker and
-- the catalog had no relationship to each other.
--
-- Keyed by the assembly name itself rather than a surrogate id, because that
-- name is already the join: a source declares `assembly = "GRCh38"`, a snapshot
-- declares a build, and a reference is chosen per assembly. Names are compared
-- exactly and deliberately not normalized, as everywhere else here — "GRCh38"
-- and "hg38" are different builds, because a false match annotates against the
-- wrong coordinates and reports nothing.
CREATE TABLE IF NOT EXISTS build (
  name        TEXT PRIMARY KEY,
  -- What to show a person choosing one. The name is an identifier; this can say
  -- "Human GRCh38 (hg38)" without that string ever being used to match.
  label       TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  -- Ordering in the picker, lowest first; ties broken by name.
  sort_order  INT  NOT NULL DEFAULT 0,
  created_at  BIGINT NOT NULL,
  updated_at  BIGINT NOT NULL
);

-- Seed from what is already registered, so an existing installation keeps
-- working without an administrator having to declare builds it plainly has.
INSERT INTO build (name, label, sort_order, created_at, updated_at)
SELECT DISTINCT s.build, s.build, 0,
       EXTRACT(EPOCH FROM now())::BIGINT, EXTRACT(EPOCH FROM now())::BIGINT
  FROM source s
 WHERE s.build <> ''
ON CONFLICT (name) DO NOTHING;

INSERT INTO build (name, label, sort_order, created_at, updated_at)
SELECT DISTINCT sn.build, sn.build, 0,
       EXTRACT(EPOCH FROM now())::BIGINT, EXTRACT(EPOCH FROM now())::BIGINT
  FROM snapshot sn
 WHERE sn.build <> ''
ON CONFLICT (name) DO NOTHING;
