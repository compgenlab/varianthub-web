-- When each caller last had a job finish, for the fair-share ordering.
--
-- The scheduler used to order queued jobs by how many the caller had *running*.
-- That signal can only separate callers up to the number of slots, and at one
-- slot it says nothing at all: by the time a worker claims, the job it just
-- finished is already marked done, so every caller has zero running, the term is
-- a constant, and the ordering collapses to plain FIFO. A caller who queued 400
-- jobs before anyone else arrived then took all 400 in a row.
--
-- The second term is "when were you last served", which measures over time rather
-- than at an instant — so it keeps working when there is only one slot and no
-- concurrency to observe. A job's effective position is
--
--   GREATEST(created_at, last_finished_at)
--
-- so finishing a job pushes the rest of that caller's queue behind everyone who
-- has been waiting, without touching those rows.
--
-- Why a wall-clock timestamp rather than an accumulating virtual-time counter:
-- it cannot run away. A counter needs a decay term to stop it climbing forever,
-- and a newcomer seeded at zero would preempt the whole queue while one seeded at
-- the current minimum needs that minimum maintained. Here the decay is
-- structural — a caller's history stops counting the moment their submit time is
-- newer than it, so GREATEST(1000, 0) is just 1000 and running long ago earns
-- nothing.
--
-- Both terms are kept. This one only advances when a job *completes*, so a caller
-- with a six-hour job in flight looks idle by this measure; the running count is
-- what stops them taking several slots at once.
CREATE TABLE IF NOT EXISTS queue_caller (
  -- The identity the scheduler is fair between: the account when signed in, the
  -- client IP otherwise. Matches COALESCE(NULLIF(user_id,''), client_ip) in the
  -- claim query.
  who              TEXT   PRIMARY KEY,
  last_finished_at BIGINT NOT NULL
);

-- Rows here are safe to drop once the timestamp is older than any queued job's
-- created_at, because GREATEST() then returns created_at regardless — an absent
-- row and a sufficiently old one are the same answer. The TTL sweep uses that to
-- keep this table from growing one row per anonymous IP forever.
CREATE INDEX IF NOT EXISTS queue_caller_age ON queue_caller (last_finished_at);
