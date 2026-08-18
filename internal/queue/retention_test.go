package queue

import (
	"context"
	"testing"
)

// The record outlives the payload.
//
// The sweep used to DELETE FROM job, and everything cascaded off it, so a week
// after a job finished there was nothing to say it had run. These pin the
// replacement: the payload goes, the record stays, and the summary keeps
// answering the questions the chunks used to.

// purgeJob finishes a job's chunks with a status and sweeps it.
func purgeJob(t *testing.T, q *Queue, jobID, status, errMsg string, nVariants int64) Job {
	t.Helper()
	ctx := context.Background()
	if _, err := q.pool.Exec(ctx,
		`UPDATE chunk SET status=$1, error=$2, n_variants=$3, finished_at=10, runner='local'
		  WHERE job_id=$4`, status, errMsg, nVariants, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PurgeOlderThan(ctx, 50); err != nil {
		t.Fatalf("purge: %v", err)
	}
	j, ok, err := q.GetJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("the job did not survive its purge: ok=%v err=%v", ok, err)
	}
	if !j.Purged() {
		t.Fatal("the job was swept but carries no purged_at")
	}
	return j
}

// The case the view is most able to get wrong. A purged job has no chunks, so
// the aggregate counts are all zero — and the existing test for success is
// n_done = n_chunks, which 0 = 0 satisfies. Read in the obvious order, every
// failed job would report success the moment its payload expired.
func TestAPurgedFailureDoesNotReadAsSuccess(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	id, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")})

	j := purgeJob(t, q, id, StatusError, "the reference was missing", 0)
	if j.Status != StatusError {
		t.Errorf("status = %q, want %q — a failure that expired must not report done",
			j.Status, StatusError)
	}
}

// Cancellation survives too, and by a different route: cancelled_at is on the
// job and outlives the chunks on its own.
func TestAPurgedCancellationIsStillACancellation(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()
	id, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")})
	if _, err := q.CancelJob(ctx, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	j := purgeJob(t, q, id, StatusDone, "", 0)
	if j.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", j.Status, StatusCancelled)
	}
}

// Marcus asked for n_variants specifically. It lives on the chunk, which the
// sweep deletes, so it only survives by being frozen first.
func TestAPurgedJobRemembersItsVariantCount(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	id, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")})

	j := purgeJob(t, q, id, StatusDone, "", 4242)
	if j.NVariants != 4242 {
		t.Errorf("n_variants = %d, want 4242 — the count did not survive the purge", j.NVariants)
	}
	if j.Status != StatusDone {
		t.Errorf("status = %q, want done", j.Status)
	}
}

// What ran it, which a record has to keep because the thing that knew is gone.
//
// Claimed for real rather than stamped by the fixture, so this covers the write
// as well as the freeze — a runner frozen faithfully from a column nothing ever
// sets would pass a test that only checked the second half.
func TestAPurgedJobRemembersWhatRanIt(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()
	id, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")})

	c, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if c.Runner != RunnerLocal {
		t.Fatalf("the claim recorded runner %q, want %q", c.Runner, RunnerLocal)
	}
	q.finish(ctx, c, StatusDone, "", Outcome{})

	if _, err := q.PurgeOlderThan(ctx, 1<<62); err != nil {
		t.Fatalf("purge: %v", err)
	}
	j, ok, _ := q.GetJob(ctx, id)
	if !ok || !j.Purged() {
		t.Fatalf("the job was not purged: ok=%v purged=%v", ok, j.Purged())
	}
	if j.Runner != RunnerLocal {
		t.Errorf("runner = %q, want %q — what ran it did not survive the purge",
			j.Runner, RunnerLocal)
	}
}

// The submitter, so usage reporting still attributes work months later — which
// is the thing the 30- and 90-day windows could never do while the rows were
// being deleted at seven.
func TestAPurgedJobStillNamesItsSubmitter(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	id, _ := submitJob(t, q, NewJob{
		Kind: KindLocus, Snapshot: "gnomad-4.1", UserID: "u1",
		ClientIP: "10.0.0.9", Origin: "api", Body: []byte("a"),
	})

	j := purgeJob(t, q, id, StatusDone, "", 7)
	if j.UserID != "u1" {
		t.Errorf("user = %q, want u1", j.UserID)
	}
	if j.Snapshot != "gnomad-4.1" {
		t.Errorf("snapshot = %q, want gnomad-4.1", j.Snapshot)
	}
	if j.Origin != "api" {
		t.Errorf("origin = %q, want api", j.Origin)
	}
	if j.CreatedAt == 0 {
		t.Error("the job forgot when it was submitted")
	}
}

// And the payload really is gone — otherwise the whole exercise keeps the
// record at the cost of never reclaiming anything.
func TestAPurgeEmptiesThePayload(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()
	id, chunkID := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")})
	if _, err := q.pool.Exec(ctx,
		`INSERT INTO chunk_result (chunk_id,vcf_uri) VALUES ($1,'file:///gone.vcf.gz')`,
		chunkID); err != nil {
		t.Fatal(err)
	}

	purgeJob(t, q, id, StatusDone, "", 3)

	for _, c := range []struct {
		what, query string
	}{
		{"chunks", `SELECT count(*) FROM chunk WHERE job_id=$1`},
		{"results", `SELECT count(*) FROM chunk_result r JOIN chunk c ON c.id=r.chunk_id WHERE c.job_id=$1`},
		{"inputs", `SELECT count(*) FROM chunk_input i JOIN chunk c ON c.id=i.chunk_id WHERE c.job_id=$1`},
		{"variants", `SELECT count(*) FROM chunk_variant v JOIN chunk c ON c.id=v.chunk_id WHERE c.job_id=$1`},
	} {
		var n int
		if err := q.pool.QueryRow(ctx, c.query, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.what, err)
		}
		if n != 0 {
			t.Errorf("%d %s row(s) survived the purge", n, c.what)
		}
	}
}

// A job still running is never touched, whatever the cutoff — the sweep is
// about finished work and an unfinished job has a payload someone is waiting on.
func TestAnUnfinishedJobIsNeverPurged(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()
	id, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")})

	if _, err := q.PurgeOlderThan(ctx, 1<<62); err != nil {
		t.Fatalf("purge: %v", err)
	}
	j, ok, _ := q.GetJob(ctx, id)
	if !ok {
		t.Fatal("the queued job disappeared")
	}
	if j.Purged() {
		t.Error("a queued job was purged; only finished work ages out")
	}
	var n int
	if err := q.pool.QueryRow(ctx, `SELECT count(*) FROM chunk WHERE job_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("the queued job's chunks were deleted")
	}
}

// Sweeping twice must not re-freeze a summary from the empty rows it left
// behind — which would overwrite a real status with whatever no-chunks derives.
func TestASecondSweepLeavesAPurgedJobAlone(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()
	id, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")})

	first := purgeJob(t, q, id, StatusError, "boom", 11)

	n, err := q.PurgeOlderThan(ctx, 1<<62)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("the second sweep purged %d already-purged job(s)", n)
	}
	again, _, _ := q.GetJob(ctx, id)
	if again.Status != first.Status || again.NVariants != first.NVariants {
		t.Errorf("a second sweep rewrote the summary: %q/%d became %q/%d",
			first.Status, first.NVariants, again.Status, again.NVariants)
	}
}
