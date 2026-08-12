-- The genes a GTF source knows about.
--
-- A cache, not a fact: everything here is re-derivable by streaming the GTF
-- again, and the file stays the authority. It exists because the two things that
-- need it cannot reach the file. The API server mounts only the config (see
-- k8s/api.yaml), so validating a pasted gene list in a form handler cannot be a
-- file read; and even where the file is reachable, scanning a 1.5 GB GENCODE GTF
-- per keystroke-to-submit is not a form interaction. The worker has the data
-- volume, so it fills this in after a GTF source is provisioned and the API
-- answers from Postgres in a single query.
--
-- Only identity is stored. Coordinates, transcripts and biotypes belong to the
-- annotation run, which reads them from the indexed file where they are already
-- fast to query; keeping a second copy here would be a gene model that can drift
-- from the one that actually annotates.
CREATE TABLE IF NOT EXISTS gtf_gene (
  source_id TEXT NOT NULL REFERENCES source (id) ON DELETE CASCADE,
  -- Version-trimmed by the writer: ENSG00000141510.17 is stored as
  -- ENSG00000141510, because the suffix counts revisions of the gene's model
  -- rather than the gene. varhub applies the same rule when a gene list matches,
  -- so a list validated against this table matches at annotate time.
  gene_id   TEXT NOT NULL,
  -- Upper-cased by the writer, and compared upper-cased. Normalizing once on the
  -- way in from the GTF and once on the way in from the form is what lets this be
  -- a plain equality join — no lower() index, no case-insensitive collation, and
  -- no second opinion about what "the same gene" means.
  gene_name TEXT NOT NULL,
  -- Keyed on gene_id rather than gene_name: a symbol is not unique in a GTF (the
  -- same name appears under two ids in GENCODE's PAR regions, and RefSeq reuses
  -- a few), while the writer guarantees one row per gene_id. Keying on the name
  -- would silently drop the second gene, and the one it dropped would depend on
  -- file order.
  PRIMARY KEY (source_id, gene_id)
);

-- The lookup a gene list is validated with. Not unique, for the reason above.
CREATE INDEX IF NOT EXISTS gtf_gene_name ON gtf_gene (source_id, gene_name);
