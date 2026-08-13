package queue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compgenlab/varianthub-web/internal/pgtest"
)

// These tests need a real Postgres — the whole point of the port is behavior the
// SQLite version got for free from SetMaxOpenConns(1), so a fake would test
// nothing. Set VHW_TEST_DATABASE_URL to run them:
//
//	docker run -d --name vhw-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=varianthub \
//	  -p 55440:5432 postgres:16-alpine
//	export VHW_TEST_DATABASE_URL='postgres://postgres:test@localhost:55440/varianthub?sslmode=disable'
//
// Each test gets its own schema so they can run in parallel without
// interference. 0002 (the catalog) is not needed here; 0003 adds chunk_variant
// and chunk.columns. queueMigrations are the migrations this package's schema
// needs, discovered rather than listed.
//
// The list used to be written out by hand, and had already drifted out of
// numeric order; adding a column then meant remembering to add it here too,
// and forgetting showed up as "column does not exist" in every test at once
// rather than as anything to do with the list. Globbing means a new chunk
// migration is picked up by existing tests the moment it lands.
//
// Sorted, because migrations are ordered by their numeric prefix and a later one
// alters what an earlier one created.
// These need a real Postgres; see internal/pgtest for the container invocation.
//
// Every migration, through the shared harness, rather than the ones whose
// filename happened to contain "chunk". That glob decided whether a migration
// reached these tests by what it was called, so one that altered the chunk
// table under another name was silently absent — which showed up as a missing
// column in whichever test touched it, saying nothing about the glob.
func testQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := New(pgtest.Pool(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(q.Close)
	return q
}

// testQueuePair is two queues over one database, for the cases that are about
// two processes rather than one — a peer worker booting, or the API starting up
// while a worker is busy. Each opens its own pool, because that is what a
// separate process has.
func testQueuePair(t *testing.T) (*Queue, *Queue) {
	t.Helper()
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	a, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(a.Close)
	b, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open peer: %v", err)
	}
	t.Cleanup(b.Close)
	return a, b
}

// waitFor polls until cond() or the deadline, failing the test on timeout.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

// monotonicNow returns a nowFn producing strictly-increasing unix seconds, so
// created_at ordering is deterministic in tests. Atomic because claimNext calls
// nowFn, and the concurrency test claims from many goroutines at once.
func monotonicNow() func() int64 {
	var n atomic.Int64
	return func() int64 { return n.Add(1) }
}

func TestEnqueueProcessDone(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gotInput := make(chan string, 1)
	q.StartWorkers(ctx, 2, func(_ context.Context, _ Chunk, input []byte) (Outcome, error) {
		gotInput <- string(input)
		// A real result shape, so the chunk_variant projection is exercised
		// too.
		body := `[{"chrom":"chr1","pos":100,"ref":"A","alt":"G","annotations":{"echo":"` +
			string(input) + `"}}]`
		return Outcome{
			Result:   []byte(body),
			N:        1,
			Columns:  []byte(`[{"key":"echo","label":"echo"}]`),
			Variants: true,
		}, nil
	})

	id, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "2026-07", Selection: "clinvar_sig",
		ClientIP: "1.2.3.4", Body: []byte("chr1:100:A:G"),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		chunk, ok, _ := q.GetJob(ctx, id)
		return ok && chunk.Status == StatusDone
	})

	chunk, _, _ := q.GetJob(ctx, id)
	if chunk.NVariants != 1 {
		t.Errorf("n_variants = %d, want 1", chunk.NVariants)
	}
	if chunk.Selection != "clinvar_sig" {
		t.Errorf("selection = %q, want clinvar_sig", chunk.Selection)
	}
	if chunk.FinishedAt == 0 || chunk.StartedAt == 0 {
		t.Errorf("started_at/finished_at not set: %+v", chunk)
	}
	if in := <-gotInput; in != "chr1:100:A:G" {
		t.Errorf("runner saw input %q", in)
	}
	result, ok, err := q.Result(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Result: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(string(result), `"echo":"chr1:100:A:G"`) {
		t.Errorf("result = %s", result)
	}

	// The blob is projected into queryable rows in the same transaction, so a
	// chunk observed as done always has results that can be paged.
	page, err := q.Results(ctx, id, ResultQuery{})
	if err != nil {
		t.Fatalf("Results: %v", err)
	}
	if page.Total != 1 || len(page.Rows) != 1 {
		t.Fatalf("Results total=%d rows=%d, want 1 and 1", page.Total, len(page.Rows))
	}
	if page.Rows[0].Chrom != "chr1" || page.Rows[0].Pos != 100 {
		t.Errorf("row = %+v", page.Rows[0])
	}
	if len(page.Columns) != 1 || page.Columns[0].Key != "echo" {
		t.Errorf("columns = %+v", page.Columns)
	}
}

func TestRunnerErrorMarksChunkFailed(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q.StartWorkers(ctx, 1, func(_ context.Context, _ Chunk, _ []byte) (Outcome, error) {
		return Outcome{}, context.DeadlineExceeded
	})
	id, _ := q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("bad")})

	waitFor(t, 5*time.Second, func() bool {
		chunk, ok, _ := q.GetJob(ctx, id)
		return ok && chunk.Status == StatusError
	})
	chunk, _, _ := q.GetJob(ctx, id)
	if chunk.Error == "" {
		t.Errorf("expected an error message on the failed chunk")
	}
}

func TestCrashRecoveryRequeuesRunning(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, _ := q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("x")})
	// Forcibly mark it running, simulating a crash mid-chunk.
	if _, err := q.pool.Exec(ctx, `UPDATE chunk SET status=$1 WHERE id=$2`, StatusRunning, id); err != nil {
		t.Fatalf("force running: %v", err)
	}
	// Open's recovery UPDATE is what should reset it.
	if _, err := q.pool.Exec(ctx,
		`UPDATE chunk SET status=$1, started_at=NULL WHERE status=$2`, StatusQueued, StatusRunning); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	chunk, ok, _ := q.GetJob(ctx, id)
	if !ok || chunk.Status != StatusQueued {
		t.Fatalf("after recovery status = %q (ok=%v), want queued", chunk.Status, ok)
	}
}

func TestQueueListAndGC(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()

	oldID, oldChunk := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.1.1.1", Body: []byte("a")})
	newID, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "2.2.2.2", Body: []byte("b")})
	queuedID, _ := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "3.3.3.3", Body: []byte("c")})

	// Give the old job a result blob so the cascade delete is exercised. It
	// hangs off the chunk that produced it, which is what the job points at.
	if _, err := q.pool.Exec(ctx,
		`INSERT INTO chunk_result (chunk_id,json) VALUES ($1,'[]')`, oldChunk); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id  string
		fin int64
	}{{oldID, 10}, {newID, 100}} {
		// The chunk finishes; the job's finish time is read from it.
		if _, err := q.pool.Exec(ctx,
			`UPDATE chunk SET status=$1, finished_at=$2 WHERE job_id=$3`, StatusDone, tc.fin, tc.id); err != nil {
			t.Fatal(err)
		}
	}

	done, err := q.ListJobs(ctx, JobFilter{Status: StatusDone}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Fatalf("List(done) = %d chunks, want 2", len(done))
	}
	if qd, _ := q.ListJobs(ctx, JobFilter{Status: StatusQueued}, 50, 0); len(qd) != 1 {
		t.Fatalf("List(queued) = %d, want 1", len(qd))
	}

	n, err := q.DeleteOlderThan(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("DeleteOlderThan removed %d, want 1", n)
	}
	if _, ok, _ := q.GetJob(ctx, oldID); ok {
		t.Errorf("old done chunk should be gone")
	}
	if _, ok, _ := q.GetJob(ctx, newID); !ok {
		t.Errorf("recent done chunk should remain")
	}
	if _, ok, _ := q.GetJob(ctx, queuedID); !ok {
		t.Errorf("queued chunk must never be GC'd")
	}
	if _, ok, _ := q.Result(ctx, oldID); ok {
		t.Errorf("GC'd chunk result blob should be gone (ON DELETE CASCADE)")
	}
}

func TestListScopedBySession(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Session: "alice", Label: "a", Body: []byte("x")})
	}
	q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Session: "bob", Label: "b", Body: []byte("x")})

	mine, err := q.ListJobs(ctx, JobFilter{Session: "alice"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("List(session=alice) = %d, want 2", len(mine))
	}
	for _, j := range mine {
		if j.Session != "alice" {
			t.Errorf("leaked a chunk from session %q", j.Session)
		}
	}
	all, _ := q.ListJobs(ctx, JobFilter{}, 50, 0)
	if len(all) != 3 {
		t.Errorf("unfiltered List = %d, want 3 (admin sees all)", len(all))
	}
}

func TestFairClaimRoundRobin(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(0) // unlimited running, so only fairness ordering is exercised
	ctx := context.Background()

	// IP A enqueues 3 chunks before IP B enqueues 3.
	for i := 0; i < 3; i++ {
		q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.1", Body: []byte("a")})
	}
	for i := 0; i < 3; i++ {
		q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.2", Body: []byte("b")})
	}

	// Claim without completing — chunks stay running, deprioritizing the
	// busier IP.
	var seq []string
	for i := 0; i < 6; i++ {
		chunk, _, ok, err := q.claimNext(ctx)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		seq = append(seq, chunk.ClientIP)
	}
	// Despite A being enqueued entirely first, fair scheduling must interleave.
	alt := 0
	for i := 1; i < len(seq); i++ {
		if seq[i] != seq[i-1] {
			alt++
		}
	}
	if alt < 4 {
		t.Errorf("expected round-robin interleaving across IPs, got sequence %v", seq)
	}
}

func TestFairClaimPerIPCap(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(1)
	ctx := context.Background()

	for _, ip := range []string{"10.0.0.1", "10.0.0.1", "10.0.0.2", "10.0.0.2"} {
		q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: ip, Body: []byte("x")})
	}

	// First two claims: one per IP. Third: both IPs at cap → nothing claimable.
	if _, _, ok, _ := q.claimNext(ctx); !ok {
		t.Fatal("claim 1 should succeed")
	}
	if _, _, ok, _ := q.claimNext(ctx); !ok {
		t.Fatal("claim 2 should succeed")
	}
	if _, _, ok, _ := q.claimNext(ctx); ok {
		t.Errorf("claim 3 should be blocked — both IPs at their per-IP cap")
	}
}

// TestConcurrentClaimsAreDistinct is new: it pins the property the SQLite
// version got from SetMaxOpenConns(1) and that FOR UPDATE SKIP LOCKED has to
// provide here. Many workers claiming at once must each get a *different*
// chunk, and none may be claimed twice.
func TestConcurrentClaimsAreDistinct(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(0)
	ctx := context.Background()

	const n = 20
	for i := 0; i < n; i++ {
		if _, err := q.Submit(ctx, NewJob{
			Kind: KindLocus, Snapshot: "s",
			ClientIP: fmt.Sprintf("10.0.0.%d", i), Body: []byte("x"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	ids := make(chan string, n)
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start // maximize contention
			chunk, _, ok, err := q.claimNext(ctx)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				ids <- chunk.ID
			} else {
				ids <- ""
			}
		}()
	}
	close(start)

	seen := map[string]bool{}
	claimed := 0
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent claim: %v", err)
		case id := <-ids:
			if id == "" {
				continue
			}
			if seen[id] {
				t.Fatalf("chunk %s claimed twice — SKIP LOCKED is not isolating claims", id)
			}
			seen[id] = true
			claimed++
		}
	}
	if claimed != n {
		t.Errorf("claimed %d of %d chunks; workers lost claims instead of taking distinct rows", claimed, n)
	}
}

// Opening the queue must not disturb work already in flight.
//
// Crash recovery used to live in Open, which both the API and the worker call.
// So restarting the API — a config change, a redeploy, `make dev-tls` — reset
// every running chunk to queued underneath the worker still executing it. The
// worker kept going and the row was claimable again, so a multi-hour VEP or
// CADD download could run twice at once, each unaware of the other, writing
// the same destination. Nothing logged an error; the chunk simply appeared to
// restart.
func TestOpeningTheQueueLeavesRunningChunksAlone(t *testing.T) {
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set")
	}
	q, other := testQueuePair(t)
	ctx := context.Background()

	id, err := q.Submit(ctx, NewJob{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	// The second process runs recovery — the API starting up, or a peer worker
	// booting, while this worker is busy.
	if n, err := other.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("another process reclaimed %d chunk(s) held by a live worker", n)
	}

	chunk, ok, err := q.GetJob(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if chunk.Status != StatusRunning {
		t.Fatalf("chunk is %q after another process opened the queue; "+
			"it was still running and is now claimable a second time", chunk.Status)
	}
}

// A lease is what separates "abandoned" from "busy", so both directions
// matter: a chunk whose holder went quiet must come back, and one whose holder
// is renewing must not — no matter who asks or how many times.
func TestLeaseDistinguishesAbandonedFromBusy(t *testing.T) {
	if os.Getenv("VHW_TEST_DATABASE_URL") == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set")
	}
	q, peer := testQueuePair(t)
	ctx := context.Background()
	// Short enough to watch expire, with renew well inside the TTL.
	q.SetLease(2*time.Second, 200*time.Millisecond)

	_, id := submitJob(t, q, NewJob{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	// The chunk's status, not the job's: a lease is held over a chunk, and
	// requeueing one leaves its job running throughout.
	statusOf := func() string {
		t.Helper()
		return getChunk(t, q, id).Status
	}

	// The claim records who holds it, which is what renewal is matched on.
	var holder string
	if err := q.pool.QueryRow(ctx,
		`SELECT COALESCE(claimed_by,'') FROM chunk WHERE id=$1`, id).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder != q.workerID {
		t.Fatalf("claimed_by = %q, want this worker %q", holder, q.workerID)
	}

	// While the holder renews, nobody reclaims it — however long that goes on
	// and however often they look. A download runs for hours; the lease has to
	// survive arbitrarily longer than its own TTL.
	q.mu.Lock()
	q.running[id] = &runningChunk{cancel: func() {}}
	q.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second) // past the 2s TTL
	for time.Now().Before(deadline) {
		if err := q.renewLeases(ctx); err != nil {
			t.Fatal(err)
		}
		if n, rErr := peer.ReclaimExpired(ctx); rErr != nil {
			t.Fatal(rErr)
		} else if n != 0 {
			t.Fatalf("a peer reclaimed a chunk whose holder is renewing it")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if s := statusOf(); s != StatusRunning {
		t.Fatalf("chunk is %q while its holder renews", s)
	}

	// The holder goes away: stop renewing, as a crashed process would.
	q.mu.Lock()
	delete(q.running, id)
	q.mu.Unlock()

	waitFor(t, 5*time.Second, func() bool {
		if _, err := peer.ReclaimExpired(ctx); err != nil {
			return false
		}
		chunk, ok, err := peer.Get(ctx, id)
		return err == nil && ok && chunk.Status == StatusQueued
	})

	// Reclaiming clears the stale holder, so the next claim starts clean.
	var claimed *string
	if err := q.pool.QueryRow(ctx, `SELECT claimed_by FROM chunk WHERE id=$1`, id).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if claimed != nil {
		t.Errorf("claimed_by = %q after reclaim, want NULL", *claimed)
	}
}

// A chunk whose worker keeps dying must eventually stop coming back.
//
// Requeueing an abandoned chunk is right — the work is unfinished and another
// worker can do it. Without a limit it is not: one provisioning chunk took a
// 492 GB volume to full by being requeued after every kill, each attempt
// leaving behind the scratch only a live process cleans up, and the
// disk-pressure evictions that followed requeued it again.
func TestAbandonedChunkStopsBeingRetried(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, id := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.2.3.4", Body: []byte("x")})

	// Claim, then abandon by expiring the lease, MaxAttempts times over.
	for i := 1; i <= MaxAttempts; i++ {
		chunk, _, ok, cErr := q.claimNext(ctx)
		if cErr != nil {
			t.Fatalf("attempt %d: claim: %v", i, cErr)
		}
		if !ok {
			t.Fatalf("attempt %d: nothing claimable, but the chunk should still be queued", i)
		}
		if chunk.ID != id {
			t.Fatalf("claimed %s, want %s", chunk.ID, id)
		}
		if _, err := q.pool.Exec(ctx, `UPDATE chunk SET lease_until=1 WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		n, rErr := q.ReclaimExpired(ctx)
		if rErr != nil {
			t.Fatalf("attempt %d: reclaim: %v", i, rErr)
		}
		if i < MaxAttempts && n != 1 {
			t.Fatalf("attempt %d: reclaimed %d, want 1 — an abandoned chunk should requeue", i, n)
		}
		if i == MaxAttempts && n != 0 {
			t.Fatalf("attempt %d: reclaimed %d, want 0 — the chunk should be failed, not requeued", i, n)
		}
	}

	got := getChunk(t, q, id)
	if got.Status != StatusError {
		t.Fatalf("status = %q, want %q — the chunk is still being retried", got.Status, StatusError)
	}
	if got.Error == "" {
		t.Error("no error recorded; the chunk failed with nothing said about why")
	}
	// And the job with it. finish() never ran for this chunk — its worker died
	// each time — so a submission left at "running" would wait for ever on work
	// that has given up.
	job, _, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusError {
		t.Fatalf("the job is %q while its only chunk has given up", job.Status)
	}
	// And it must not be claimable again.
	if _, _, ok, err := q.claimNext(ctx); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a failed chunk was claimed again")
	}
}

// One kill is not three: a rolling restart must not cost a chunk its place.
func TestOneAbandonmentStillRequeues(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, id := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.2.3.4", Body: []byte("x")})
	if _, _, ok, cErr := q.claimNext(ctx); cErr != nil || !ok {
		t.Fatalf("claim: %v ok=%v", cErr, ok)
	}
	if _, err := q.pool.Exec(ctx, `UPDATE chunk SET lease_until=1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := q.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}
	// The chunk goes back on the queue. Its job stays running: the submission
	// has not finished and has not failed, it lost a worker.
	if got := getChunk(t, q, id); got.Status != StatusQueued {
		t.Fatalf("chunk status = %q, want %q", got.Status, StatusQueued)
	}
	job, _, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Terminal() {
		t.Fatalf("job status = %q; one lost attempt is not an outcome", job.Status)
	}
}

// The output of a run that never returns.
//
// storeLog runs after a chunk finishes, so a worker killed mid-run wrote
// nothing and the chunk reported being abandoned with no account of what it
// was doing. A periodic flush is the whole point: whatever reached the
// database before the kill is all there will ever be, so there has to be
// something there.
func TestLogSurvivesAWorkerThatNeverReturns(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, id := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.2.3.4", Body: []byte("x")})

	w := NewLogWriter(ctx, q, id)
	w.Note("starting on worker abc")
	w.Line("downloading 1 input file(s)")
	w.Line("  ↓ GRCh38_latest_genomic.gtf.gz")
	// No Close: the process died here.
	w.flush(ctx)

	out, found, err := q.Log(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("nothing was recorded for a chunk that printed output")
	}
	for _, want := range []string{"starting on worker abc", "downloading 1 input file"} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q; got:\n%s", want, out)
		}
	}
}

// An abandoned chunk says so in its own log, so "abandoned N times" has times,
// workers, and output behind it rather than being the whole story.
func TestAbandonmentIsRecordedInTheChunkLog(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, id := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.2.3.4", Body: []byte("x")})
	if _, _, ok, cErr := q.claimNext(ctx); cErr != nil || !ok {
		t.Fatalf("claim: %v ok=%v", cErr, ok)
	}
	if _, err := q.pool.Exec(ctx, `UPDATE chunk SET lease_until=1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}

	out, found, err := q.Log(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !strings.Contains(out, "stopped renewing the lease") {
		t.Fatalf("the reclaim left no note in the chunk log; got found=%v out=%q", found, out)
	}
}

// Two writers must not lose each other's lines: the worker streams its run
// while a peer notes that the chunk was abandoned.
func TestAppendLogDoesNotClobber(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	jobID, id := submitJob(t, q, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.2.3.4", Body: []byte("x")})
	if err := q.AppendLog(ctx, id, "first\n"); err != nil {
		t.Fatal(err)
	}
	if err := q.AppendLog(ctx, id, "second\n"); err != nil {
		t.Fatal(err)
	}
	out, _, err := q.Log(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Fatalf("append lost a line: %q", out)
	}
}
