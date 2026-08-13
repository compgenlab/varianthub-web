-- Where a job's answer-as-a-VCF is stored, once it has been built.
--
-- A VCF submission is answered with the submitter's own file annotated, which
-- until now was assembled on every download: read the stored input, stream the
-- result rows, merge, write. That is the whole file parsed and rewritten per
-- request, and it is why the input had to be kept for as long as the results
-- were downloadable.
--
-- Building it once when the job finishes moves that cost to where it is paid
-- anyway, and it is what lets the input be deleted at completion rather than at
-- expiry — the input is only needed to produce this.
--
-- Nullable, and it stays nullable. A locus job has no submitted file to merge
-- onto, a job that finished before this existed has no object, and a merge that
-- fails must not fail the job it belongs to — its results are still correct and
-- still downloadable by the older path. Absent means "render it the old way",
-- which is a fallback rather than an error.
ALTER TABLE job_result ADD COLUMN IF NOT EXISTS vcf_uri TEXT;

-- json becomes optional too.
--
-- Not used yet. It is here because the next thing to move out of Postgres is the
-- result blob itself — for 2.6M variants it is gigabytes of JSON in a row — and
-- doing the column change now means that migration is a code change rather than
-- another schema change against a table under load.
ALTER TABLE job_result ALTER COLUMN json DROP NOT NULL;
