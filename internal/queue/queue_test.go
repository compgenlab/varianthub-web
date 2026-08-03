package queue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need a real Postgres — the whole point of the port is behavior the
// SQLite version got for free from SetMaxOpenConns(1), so a fake would test
// nothing. Set VHW_TEST_DATABASE_URL to run them:
//
//	docker run -d --name vhw-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=varianthub \
//	  -p 55440:5432 postgres:16-alpine
//	export VHW_TEST_DATABASE_URL='postgres://postgres:test@localhost:55440/varianthub?sslmode=disable'
//
// Each test gets its own schema so they can run in parallel without interference.
// 0002 (the catalog) is not needed here; 0003 adds job_variant and job.columns.
var migrationFiles = []string{
	"../../migrations/0001_job_queue.sql",
	"../../migrations/0003_job_variant.sql",
	"../../migrations/0010_job_user.sql",
	"../../migrations/0013_job_log.sql",
}

func testQueue(t *testing.T) *Queue {
	t.Helper()
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping Postgres queue tests")
	}
	ctx := context.Background()

	var ddl strings.Builder
	for _, f := range migrationFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ddl.Write(b)
		ddl.WriteString("\n")
	}

	// Isolate this test in its own schema, and get the schema in place *before*
	// Open — Open runs a crash-recovery UPDATE against job on connect.
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	setup, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := setup.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		setup.Close()
		t.Fatalf("create schema: %v", err)
	}
	if _, err := setup.Exec(ctx, `SET search_path TO `+schema+`; `+ddl.String()); err != nil {
		setup.Close()
		t.Fatalf("apply migration: %v", err)
	}
	setup.Close()

	q, err := Open(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		drop, err := pgxpool.New(context.Background(), dsn)
		if err == nil {
			_, _ = drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			drop.Close()
		}
		q.Close()
	})
	return q
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
	q.StartWorkers(ctx, 2, func(_ context.Context, _ Job, input []byte) (Outcome, error) {
		gotInput <- string(input)
		// A real result shape, so the job_variant projection is exercised too.
		body := `[{"chrom":"chr1","pos":100,"ref":"A","alt":"G","annotations":{"echo":"` +
			string(input) + `"}}]`
		return Outcome{
			Result:   []byte(body),
			N:        1,
			Columns:  []byte(`[{"key":"echo","label":"echo"}]`),
			Variants: true,
		}, nil
	})

	id, err := q.Enqueue(ctx, NewJob{
		Kind: KindLocus, Snapshot: "2026-07", Selection: "clinvar_sig",
		ClientIP: "1.2.3.4", Body: []byte("chr1:100:A:G"),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		job, ok, _ := q.Get(ctx, id)
		return ok && job.Status == StatusDone
	})

	job, _, _ := q.Get(ctx, id)
	if job.NVariants != 1 {
		t.Errorf("n_variants = %d, want 1", job.NVariants)
	}
	if job.Selection != "clinvar_sig" {
		t.Errorf("selection = %q, want clinvar_sig", job.Selection)
	}
	if job.FinishedAt == 0 || job.StartedAt == 0 {
		t.Errorf("started_at/finished_at not set: %+v", job)
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

	// The blob is projected into queryable rows in the same transaction, so a job
	// observed as done always has results that can be paged.
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

func TestRunnerErrorMarksJobFailed(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q.StartWorkers(ctx, 1, func(_ context.Context, _ Job, _ []byte) (Outcome, error) {
		return Outcome{}, context.DeadlineExceeded
	})
	id, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("bad")})

	waitFor(t, 5*time.Second, func() bool {
		job, ok, _ := q.Get(ctx, id)
		return ok && job.Status == StatusError
	})
	job, _, _ := q.Get(ctx, id)
	if job.Error == "" {
		t.Errorf("expected an error message on the failed job")
	}
}

func TestCrashRecoveryRequeuesRunning(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("x")})
	// Forcibly mark it running, simulating a crash mid-job.
	if _, err := q.pool.Exec(ctx, `UPDATE job SET status=$1 WHERE id=$2`, StatusRunning, id); err != nil {
		t.Fatalf("force running: %v", err)
	}
	// Open's recovery UPDATE is what should reset it.
	if _, err := q.pool.Exec(ctx,
		`UPDATE job SET status=$1, started_at=NULL WHERE status=$2`, StatusQueued, StatusRunning); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	job, ok, _ := q.Get(ctx, id)
	if !ok || job.Status != StatusQueued {
		t.Fatalf("after recovery status = %q (ok=%v), want queued", job.Status, ok)
	}
}

func TestWaitForReturnsOnDone(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.StartListener(ctx)

	q.StartWorkers(ctx, 1, func(_ context.Context, _ Job, _ []byte) (Outcome, error) {
		time.Sleep(50 * time.Millisecond)
		return Outcome{Result: []byte(`[]`)}, nil
	})
	id, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("x")})

	start := time.Now()
	job, ok, err := q.WaitFor(ctx, id, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("WaitFor: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusDone {
		t.Errorf("status = %q, want done", job.Status)
	}
	// The NOTIFY should wake it well before the 2s safety poll.
	if elapsed := time.Since(start); elapsed > waitPoll {
		t.Errorf("WaitFor took %s — NOTIFY path likely not working", elapsed)
	}
}

func TestWaitForTimeoutReturnsCurrent(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	// No workers: the job stays queued.
	id, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("x")})

	start := time.Now()
	job, ok, err := q.WaitFor(ctx, id, 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("WaitFor: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusQueued {
		t.Errorf("status = %q, want queued", job.Status)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("WaitFor returned after %s, expected to wait ~200ms", elapsed)
	}
}

func TestWaitForUnknownID(t *testing.T) {
	q := testQueue(t)
	_, ok, err := q.WaitFor(context.Background(), "nope", time.Second)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if ok {
		t.Errorf("ok = true for an unknown job id")
	}
}

func TestQueueListAndGC(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()

	oldID, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.1.1.1", Body: []byte("a")})
	newID, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "2.2.2.2", Body: []byte("b")})
	queuedID, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "3.3.3.3", Body: []byte("c")})

	// Give the old job a result blob so the cascade delete is exercised.
	if _, err := q.pool.Exec(ctx,
		`INSERT INTO job_result (job_id,json) VALUES ($1,'[]')`, oldID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id  string
		fin int64
	}{{oldID, 10}, {newID, 100}} {
		if _, err := q.pool.Exec(ctx,
			`UPDATE job SET status=$1, finished_at=$2 WHERE id=$3`, StatusDone, tc.fin, tc.id); err != nil {
			t.Fatal(err)
		}
	}

	done, err := q.List(ctx, JobFilter{Status: StatusDone}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Fatalf("List(done) = %d jobs, want 2", len(done))
	}
	if qd, _ := q.List(ctx, JobFilter{Status: StatusQueued}, 50, 0); len(qd) != 1 {
		t.Fatalf("List(queued) = %d, want 1", len(qd))
	}

	n, err := q.DeleteOlderThan(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("DeleteOlderThan removed %d, want 1", n)
	}
	if _, ok, _ := q.Get(ctx, oldID); ok {
		t.Errorf("old done job should be gone")
	}
	if _, ok, _ := q.Get(ctx, newID); !ok {
		t.Errorf("recent done job should remain")
	}
	if _, ok, _ := q.Get(ctx, queuedID); !ok {
		t.Errorf("queued job must never be GC'd")
	}
	if _, ok, _ := q.Result(ctx, oldID); ok {
		t.Errorf("GC'd job result blob should be gone (ON DELETE CASCADE)")
	}
}

func TestListScopedBySession(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Session: "alice", Label: "a", Body: []byte("x")})
	}
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Session: "bob", Label: "b", Body: []byte("x")})

	mine, err := q.List(ctx, JobFilter{Session: "alice"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("List(session=alice) = %d, want 2", len(mine))
	}
	for _, j := range mine {
		if j.Session != "alice" {
			t.Errorf("leaked a job from session %q", j.Session)
		}
	}
	all, _ := q.List(ctx, JobFilter{}, 50, 0)
	if len(all) != 3 {
		t.Errorf("unfiltered List = %d, want 3 (admin sees all)", len(all))
	}
}

func TestFairClaimRoundRobin(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(0) // unlimited running, so only fairness ordering is exercised
	ctx := context.Background()

	// IP A enqueues 3 jobs before IP B enqueues 3.
	for i := 0; i < 3; i++ {
		q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.1", Body: []byte("a")})
	}
	for i := 0; i < 3; i++ {
		q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.2", Body: []byte("b")})
	}

	// Claim without completing — jobs stay running, deprioritizing the busier IP.
	var seq []string
	for i := 0; i < 6; i++ {
		job, _, ok, err := q.claimNext(ctx)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		seq = append(seq, job.ClientIP)
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
		q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: ip, Body: []byte("x")})
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

// TestConcurrentClaimsAreDistinct is new: it pins the property the SQLite version
// got from SetMaxOpenConns(1) and that FOR UPDATE SKIP LOCKED has to provide here.
// Many workers claiming at once must each get a *different* job, and none may be
// claimed twice.
func TestConcurrentClaimsAreDistinct(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(0)
	ctx := context.Background()

	const n = 20
	for i := 0; i < n; i++ {
		if _, err := q.Enqueue(ctx, NewJob{
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
			job, _, ok, err := q.claimNext(ctx)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				ids <- job.ID
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
				t.Fatalf("job %s claimed twice — SKIP LOCKED is not isolating claims", id)
			}
			seen[id] = true
			claimed++
		}
	}
	if claimed != n {
		t.Errorf("claimed %d of %d jobs; workers lost claims instead of taking distinct rows", claimed, n)
	}
}
