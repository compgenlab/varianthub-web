package queue

import (
	"context"
	"testing"
)

// submitOne submits a trivial locus job and returns the job's id.
func submitOne(t *testing.T, q *Queue, user string) string {
	t.Helper()
	id, err := q.Submit(context.Background(), NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: user, Body: []byte("chr1:1:A:T"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// enqueueOne submits a trivial locus job and returns its one chunk's id.
//
// Most of this package's tests are about what a worker claims, which is a
// chunk; the job around it is scaffolding they need to exist and not to name.
func enqueueOne(t *testing.T, q *Queue, user string) string {
	t.Helper()
	chunks, err := q.JobChunks(context.Background(), submitOne(t, q, user))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("a locus submission became %d chunks, want 1", len(chunks))
	}
	return chunks[0].ID
}

// submitJob submits n and returns both ids: the job's, which a caller holds,
// and its one chunk's, which is what a worker claims and what the lease,
// attempt and log rows hang off.
//
// Both, because the two are separate strings now and a test that reaches for
// the wrong one fails in a way that looks like the behaviour under test.
func submitJob(t *testing.T, q *Queue, n NewJob) (jobID, chunkID string) {
	t.Helper()
	ctx := context.Background()
	jobID, err := q.Submit(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := q.JobChunks(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("the submission became %d chunks, want 1", len(chunks))
	}
	return jobID, chunks[0].ID
}

// getChunk reads a chunk row by id.
func getChunk(t *testing.T, q *Queue, id string) Chunk {
	t.Helper()
	c, err := scanChunk(q.pool.QueryRow(context.Background(),
		`SELECT `+chunkCols+` FROM chunk WHERE id=$1`, id))
	if err != nil {
		t.Fatalf("read chunk %s: %v", id, err)
	}
	return c
}

// finishByID records an outcome for a chunk named by id, the way a worker's
// process would for one it is holding.
func finishByID(t *testing.T, q *Queue, id, status, errMsg string, out Outcome) {
	t.Helper()
	q.finish(context.Background(), getChunk(t, q, id), status, errMsg, out)
}

// claimAndAbandon simulates a worker being killed: it claims whatever is next,
// lets that chunk's lease lapse, and sweeps — which is what a killed pod
// leaves behind. Returns the chunk it actually claimed.
//
// It expires the claimed id rather than one the caller names, because an
// abandoned chunk goes back on the queue: after the first abandonment the next
// claim may well pick the same chunk up again rather than the one just
// enqueued. Assuming otherwise expires a queued chunk, which the sweep
// correctly ignores, and the test then measures nothing.
func claimAndAbandon(t *testing.T, q *Queue) string {
	t.Helper()
	ctx := context.Background()
	chunk, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	expire(t, q, chunk.ID)
	if _, err := q.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	return chunk.ID
}

// Claiming opens an attempt; finishing closes it with the outcome.
func TestAnAttemptIsOpenedOnClaimAndClosedOnFinish(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := enqueueOne(t, q, "u")

	chunk, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}

	open, err := q.ChunkAttempts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("claiming recorded %d attempts, want 1", len(open))
	}
	if open[0].Worker != q.WorkerID() {
		t.Errorf("attempt worker = %q, want %q", open[0].Worker, q.WorkerID())
	}
	if open[0].StartedAt == 0 {
		t.Error("attempt has no start time")
	}
	// Still in flight: an outcome now would mean the row is closed before the
	// work is, and "how long has this attempt been running" is exactly the
	// question asked of a chunk that seems stuck.
	if open[0].Outcome != "" || open[0].EndedAt != 0 {
		t.Errorf("attempt closed while still running: %+v", open[0])
	}

	q.finish(ctx, chunk, StatusDone, "", Outcome{})

	done, err := q.ChunkAttempts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Fatalf("finishing changed the attempt count to %d", len(done))
	}
	if done[0].Outcome != OutcomeDone {
		t.Errorf("outcome = %q, want %q", done[0].Outcome, OutcomeDone)
	}
	if done[0].EndedAt == 0 {
		t.Error("a finished attempt has no end time")
	}
}

// The attempt number tracks chunk.attempts, so the two records can be
// reconciled. If they drift, one of them is wrong and there is no way to tell
// which.
func TestTheAttemptNumberMatchesTheChunkCounter(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := enqueueOne(t, q, "u")

	claimAndAbandon(t, q)
	claimAndAbandon(t, q)

	attempts, err := q.ChunkAttempts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("recorded %d attempts, want 2", len(attempts))
	}
	var counter int
	if err := q.pool.QueryRow(ctx, `SELECT attempts FROM chunk WHERE id=$1`, id).
		Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if got := attempts[len(attempts)-1].N; got != counter {
		t.Errorf("last attempt is #%d but chunk.attempts = %d; the two records disagree",
			got, counter)
	}
}

// The row this table exists for: after the sweep, chunk.claimed_by is NULL and
// the only surviving record of which process died is the attempt.
func TestAnAbandonedAttemptKeepsTheWorkerTheChunkRowLoses(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := enqueueOne(t, q, "u")
	worker := q.WorkerID()

	claimAndAbandon(t, q)

	// The chunk row has genuinely forgotten.
	var claimedBy *string
	if err := q.pool.QueryRow(ctx, `SELECT claimed_by FROM chunk WHERE id=$1`, id).
		Scan(&claimedBy); err != nil {
		t.Fatal(err)
	}
	if claimedBy != nil && *claimedBy != "" {
		t.Fatalf("claimed_by = %q; this test is not testing what it thinks", *claimedBy)
	}

	attempts, err := q.ChunkAttempts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(attempts))
	}
	a := attempts[0]
	if a.Worker != worker {
		t.Errorf("the abandoned attempt names %q, want %q — the identity is lost", a.Worker, worker)
	}
	if !a.Abandoned() {
		t.Errorf("outcome = %q, want %q", a.Outcome, OutcomeAbandoned)
	}
	if a.EndedAt == 0 {
		t.Error("an abandoned attempt was left open; nothing would ever close it")
	}
}

// Each attempt keeps its own error. A chunk that fails differently every time
// is a different diagnosis from one that fails the same way three times, and
// chunk.error holds only the last.
func TestEachAttemptKeepsItsOwnError(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := enqueueOne(t, q, "u")

	// Abandoned once, then a genuine failure.
	claimAndAbandon(t, q)
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("re-claim: %v ok=%v", err, ok)
	}
	finishByID(t, q, id, StatusError, "reference FASTA missing", Outcome{})

	attempts, err := q.ChunkAttempts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("recorded %d attempts, want 2", len(attempts))
	}
	if !attempts[0].Abandoned() {
		t.Errorf("attempt 1 outcome = %q, want %q", attempts[0].Outcome, OutcomeAbandoned)
	}
	if attempts[0].Error != "" {
		t.Errorf("an abandoned attempt reported an error (%q); its worker never got to",
			attempts[0].Error)
	}
	if attempts[1].Outcome != OutcomeError {
		t.Errorf("attempt 2 outcome = %q, want %q", attempts[1].Outcome, OutcomeError)
	}
	if attempts[1].Error != "reference FASTA missing" {
		t.Errorf("attempt 2 error = %q, want the failure it reported", attempts[1].Error)
	}
}

// The miscount the attempt record fixes.
//
// AbandonedExhausted used to be "status = error AND attempts >= MaxAttempts",
// which cannot distinguish a chunk that ran out of attempts from one that was
// abandoned a couple of times and then genuinely failed. The two want opposite
// responses — more memory for the worker, versus a bad input — and the counter
// reported them identically.
func TestAChunkThatFailsOnItsLastAttemptIsNotCountedAsAbandoned(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := enqueueOne(t, q, "u")

	// Lose MaxAttempts-1 workers, then fail for real on the last attempt.
	for i := 0; i < MaxAttempts-1; i++ {
		claimAndAbandon(t, q)
	}
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("final claim: %v ok=%v", err, ok)
	}
	finishByID(t, q, id, StatusError, "bad VCF header", Outcome{})

	// The precondition the old counter tripped on.
	var counter int
	if err := q.pool.QueryRow(ctx, `SELECT attempts FROM chunk WHERE id=$1`, id).
		Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if counter < MaxAttempts {
		t.Fatalf("attempts = %d; the old counter's predicate would not have fired", counter)
	}

	st, err := q.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.AbandonedExhausted != 0 {
		t.Errorf("abandoned_exhausted = %d; this chunk failed on its own, it did not run out of attempts",
			st.AbandonedExhausted)
	}
	if st.Failed != 1 {
		t.Errorf("failed = %d, want 1", st.Failed)
	}
	// The abandonments still happened and are still counted as attempts — the
	// chunk-level counter is what changed, not the history.
	if st.AbandonedAttempts24h != int64(MaxAttempts-1) {
		t.Errorf("abandoned_attempts_24h = %d, want %d", st.AbandonedAttempts24h, MaxAttempts-1)
	}
}

// A rate, not a stock: a deployment losing an attempt regularly but retrying
// successfully leaves no chunk in a bad state, so both chunk-level counters
// read zero while something is plainly wrong.
func TestAbandonmentsAreCountedEvenWhenTheChunkLaterSucceeds(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := enqueueOne(t, q, "u")

	claimAndAbandon(t, q)
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("re-claim: %v ok=%v", err, ok)
	}
	finishByID(t, q, id, StatusDone, "", Outcome{})

	st, err := q.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Succeeded != 1 {
		t.Fatalf("succeeded = %d, want 1", st.Succeeded)
	}
	if st.AbandonedRetrying != 0 || st.AbandonedExhausted != 0 {
		t.Errorf("the chunk-level counters should be clear: retrying=%d exhausted=%d",
			st.AbandonedRetrying, st.AbandonedExhausted)
	}
	if st.AbandonedAttempts24h != 1 {
		t.Errorf("abandoned_attempts_24h = %d, want 1 — the lost worker left no other trace",
			st.AbandonedAttempts24h)
	}
}

// "Is one worker losing all of them?" — the question chunk.attempts cannot
// answer, because a counter of 2 looks the same whether one process lost both
// or two processes lost one each.
func TestWorkerHealthAttributesAbandonmentToTheProcessThatLostIt(t *testing.T) {
	a, b := testQueuePair(t)
	ctx := context.Background()

	// "a" loses two attempts; "b" makes one and reports on it. Which chunk
	// each picks up does not matter — the question is which *worker* the
	// losses are attributed to.
	enqueueOne(t, a, "u1")
	claimAndAbandon(t, a)
	enqueueOne(t, a, "u2")
	claimAndAbandon(t, a)

	enqueueOne(t, b, "u3")
	claimed, _, ok, err := b.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("peer claim: %v ok=%v", err, ok)
	}
	// The chunk b actually claimed, not the one just enqueued: finishing some
	// other chunk would close no attempt and leave b's own still open, and the
	// assertion below would pass without meaning anything.
	b.finish(ctx, claimed, StatusDone, "", Outcome{})

	health, err := a.WorkerHealthSince(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	byWorker := map[string]WorkerHealth{}
	for _, h := range health {
		byWorker[h.Worker] = h
	}
	if got := byWorker[a.WorkerID()].Abandoned; got != 2 {
		t.Errorf("worker a abandoned %d, want 2 (health: %+v)", got, health)
	}
	if got := byWorker[b.WorkerID()].Abandoned; got != 0 {
		t.Errorf("worker b abandoned %d, want 0 — it reported on everything it took", got)
	}
	if got := byWorker[b.WorkerID()].Attempts; got != 1 {
		t.Errorf("worker b made %d attempts, want 1", got)
	}
	// Worst first, so the process to look at is the one at the top.
	if len(health) > 0 && health[0].Worker != a.WorkerID() {
		t.Errorf("health is ordered %q first; the worst offender should lead", health[0].Worker)
	}
}

// Attempts are history *of a chunk* and go when it does. Without the cascade
// the TTL sweep would leave rows referencing chunks that no longer exist,
// growing without bound.
func TestAttemptsAreDeletedWithTheChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := enqueueOne(t, q, "u")

	// Abandoned once and then finished, so there are two attempts *and* the
	// chunk is terminal. The sweep only collects chunks with a finished_at, so
	// a chunk left queued after a reclaim keeps its history — correctly, since
	// the chunk itself is still there.
	claimAndAbandon(t, q)
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("re-claim: %v ok=%v", err, ok)
	}
	finishByID(t, q, id, StatusDone, "", Outcome{})

	var n int
	if err := q.pool.QueryRow(ctx, `SELECT count(*) FROM chunk_attempt`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no attempt was recorded; this test would pass vacuously")
	}

	if _, err := q.DeleteOlderThan(ctx, 1<<62); err != nil {
		t.Fatal(err)
	}
	if err := q.pool.QueryRow(ctx, `SELECT count(*) FROM chunk_attempt`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d attempt row(s) outlived the chunk they describe", n)
	}
}
