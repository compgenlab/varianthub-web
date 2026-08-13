-- A submission too large for one job, and the jobs it became.
--
-- A VCF past the chunk size is split into pieces that annotate independently
-- and are joined back at the end. Chromosome 22 of a WGS cohort is 2.6M
-- variants — 26 chunks at 100,000 — and doing that as one job means one process
-- holding one worker for hours with no progress to report and everything lost
-- if it dies.
--
-- An entity rather than a chain. The first design linked each job to the next,
-- which answers "what comes after this one" and nothing else: how many are
-- there, how many are left, has it finished. Those are the questions a caller
-- and the collect step both ask, and a row that holds them answers all three
-- without walking anything.
CREATE TABLE IF NOT EXISTS batch (
  id       TEXT PRIMARY KEY,
  -- The job the submitter was given and still polls. It is the split job: it
  -- exists before any chunk does, so there is something to return from the
  -- request that created it, and something to show while the split is running.
  job_id   TEXT NOT NULL REFERENCES job (id) ON DELETE CASCADE,

  -- How many chunks the split produced, and how many have finished.
  --
  -- Counted rather than derived from the jobs. The collect step has to start
  -- exactly once, and "are they all done" asked as a query over sibling rows is
  -- a question two chunks finishing together can both answer yes to. Bumping a
  -- counter and reading the result in one statement cannot be raced.
  --
  -- chunks is 0 until the split finishes, which is what "pending" means here.
  chunks   INTEGER NOT NULL DEFAULT 0,
  done     INTEGER NOT NULL DEFAULT 0,
  -- Set when a chunk fails. The batch keeps going — the other chunks are
  -- already running and killing them wastes work that may be all a caller
  -- wanted — but collect refuses to assemble a file with a hole in it.
  failed   INTEGER NOT NULL DEFAULT 0,

  -- Where the pieces live: <job_storage>/jobs/<split-job-id>. The chunks are
  -- under it, so one prefix names everything the batch owns and the storage
  -- sweep needs no special case for them.
  prefix   TEXT   NOT NULL,
  created_at BIGINT NOT NULL
);

-- A job's place in its batch.
--
-- Nullable because most jobs are not in one, and 0 is a real chunk index rather
-- than a sentinel — the first chunk carries the header, so which one is chunk 0
-- decides what the joined file looks like.
ALTER TABLE job ADD COLUMN IF NOT EXISTS batch_id    TEXT;
ALTER TABLE job ADD COLUMN IF NOT EXISTS chunk_index INTEGER;

-- The chunks of one batch, in order. Read by collect, which must join them in
-- the order they were split or the output is a VCF whose records go backwards.
CREATE INDEX IF NOT EXISTS job_batch ON job (batch_id, chunk_index)
  WHERE batch_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS batch_job ON batch (job_id);
