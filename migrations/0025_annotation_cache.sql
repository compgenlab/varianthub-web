-- The shared annotation cache.
--
-- The unit of caching is a (variant, source) pair: what one pinned source has to
-- say about one variant. That is the unit a job asks for, the unit that is wholly
-- present or wholly absent, and therefore the unit eviction removes. Values hang
-- off it.
--
-- Only what is a pure function of the variant belongs here. A value that reads a
-- sample's FORMAT fields, or its neighbours, is not the same for two callers who
-- ask about the same locus, and caching one would serve one sample's number as
-- another's — wrong, plausible, and invisible in the result. The writer decides
-- what qualifies; the schema cannot.
--
-- Why not one flat table with last_used on every row. Postgres updates are
-- copy-on-write, so touching a variant's timestamp would rewrite one tuple per
-- annotation field, on the hottest table, on every read — bloat and vacuum
-- pressure scaling with read traffic, which is the wrong thing to scale with.
-- One parent row per (variant, source) means one rewrite instead of N.
--
-- Why a surrogate key. The natural key is six columns; repeated in the value
-- table and its index it dominates storage at the tens of millions of rows this
-- is sized for. An 8-byte id costs one join and saves that.
CREATE TABLE IF NOT EXISTS cache_variant_source (
  id       BIGSERIAL PRIMARY KEY,
  assembly TEXT   NOT NULL,
  chrom    TEXT   NOT NULL,
  pos      BIGINT NOT NULL,
  ref      TEXT   NOT NULL,
  alt      TEXT   NOT NULL,
  -- "name:version" of the source, matching catalog.Source.Ref(): a version is
  -- part of the identity, so a source re-provisioned at a new version cannot
  -- serve values computed from data it no longer has. A manifest edit changes
  -- what a source emits without changing its version, so that path purges
  -- instead (see catalog.UpdateSourceTOML).
  source   TEXT   NOT NULL,
  -- Unix seconds, rounded down to the hour by the writer. LRU does not need
  -- second precision, and rounding turns a write per read into a write per
  -- variant per hour.
  last_used BIGINT NOT NULL,
  UNIQUE (assembly, chrom, pos, ref, alt, source)
);

-- Eviction reads this in order; without it the sweep sorts the whole table.
CREATE INDEX IF NOT EXISTS cache_variant_source_lru
  ON cache_variant_source (last_used);

-- Purging one source's entries after a manifest edit. Without it that purge is a
-- sequential scan of the whole cache, on the admin's save.
CREATE INDEX IF NOT EXISTS cache_variant_source_source
  ON cache_variant_source (source);

-- One annotation's value. Keyed by the manifest's name for the field, not the
-- prefixed name a snapshot emits, so changing annotation_prefix renames output
-- without invalidating everything cached under the old name.
--
-- A parent with no rows here is meaningful: "this source was asked about this
-- variant and had nothing to say". Without that state, a common variant no
-- source annotates is recomputed forever.
CREATE TABLE IF NOT EXISTS cache_entry (
  vs_id      BIGINT NOT NULL REFERENCES cache_variant_source (id) ON DELETE CASCADE,
  key        TEXT   NOT NULL,
  value_text TEXT,
  value_num  DOUBLE PRECISION,
  PRIMARY KEY (vs_id, key)
);
