package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	// A realistic epoch: "30 days ago" must stay positive, or the helper below
	// reads it as "never finished".
	now := int64(1800000000)
	q.nowFn = func() int64 { return now }

	// created_at/finished_at are written directly: the point is the aggregate,
	// and driving it through the scheduler would fix the timestamps to real time.
	insert := func(id, status string, variants int64, created, finished int64) {
		t.Helper()
		var fin any
		if finished > 0 {
			fin = finished
		}
		if _, err := q.pool.Exec(ctx, `
			INSERT INTO job (id,kind,snapshot,selection,status,n_variants,created_at,finished_at)
			VALUES ($1,'locus','snap','',$2,$3,$4,$5)`,
			id, status, variants, created, fin); err != nil {
			t.Fatal(err)
		}
	}

	insert("d1", StatusDone, 10, now-3600, now-3000)           // within 24h
	insert("d2", StatusDone, 32, now-3*24*3600, now-3*24*3600) // within 7d, not 24h
	insert("d3", StatusDone, 5, now-30*24*3600, now-30*24*3600)
	// A failed job's n_variants is what was submitted, not what was annotated.
	insert("e1", StatusError, 999, now-7200, now-7000)
	insert("q1", StatusQueued, 3, now-600, 0)
	insert("q2", StatusQueued, 4, now-1200, 0) // the longest wait
	insert("r1", StatusRunning, 7, now-300, 0)

	s, err := q.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 7 {
		t.Errorf("total = %d, want 7", s.Total)
	}
	if s.Succeeded != 3 || s.Failed != 1 {
		t.Errorf("succeeded/failed = %d/%d, want 3/1", s.Succeeded, s.Failed)
	}
	if s.Queued != 2 || s.Running != 1 {
		t.Errorf("queued/running = %d/%d, want 2/1", s.Queued, s.Running)
	}
	if s.OldestQueuedAt != now-1200 {
		t.Errorf("oldest queued = %d, want %d", s.OldestQueuedAt, now-1200)
	}
	// 10+32+5 — the failed job's 999 must not be counted as annotated.
	if s.Variants != 47 {
		t.Errorf("variants = %d, want 47 (successful jobs only)", s.Variants)
	}
	if s.Last24h != 2 {
		t.Errorf("last 24h = %d, want 2", s.Last24h)
	}
	if s.Last7d != 3 {
		t.Errorf("last 7d = %d, want 3", s.Last7d)
	}
}

// An empty deployment reports zeroes, not an error and not a null scan.
func TestStatsOnEmptyQueue(t *testing.T) {
	q := testQueue(t)
	s, err := q.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s != (Stats{}) {
		t.Errorf("empty queue reports %+v", s)
	}
}

// A run's output is kept so it can be read without shell access to the worker,
// and survives the container that produced it.
func TestJobLog(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewJob{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing recorded yet is distinguishable from a run that printed nothing.
	out, found, err := q.Log(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if found || out != "" {
		t.Errorf("a fresh job reports a log: %q (found=%v)", out, found)
	}

	if err := q.SetLog(ctx, id, "varhub: fetching\nvarhub: done\n"); err != nil {
		t.Fatal(err)
	}
	out, found, err = q.Log(ctx, id)
	if err != nil || !found {
		t.Fatalf("Log = %q, %v, %v", out, found, err)
	}
	if !strings.Contains(out, "fetching") {
		t.Errorf("output = %q", out)
	}

	// A retry replaces rather than appends: the log describes the run that is
	// recorded on the job, and two runs' output interleaved describes neither.
	if err := q.SetLog(ctx, id, "second run\n"); err != nil {
		t.Fatal(err)
	}
	out, _, _ = q.Log(ctx, id)
	if strings.Contains(out, "fetching") || !strings.Contains(out, "second run") {
		t.Errorf("after a second run: %q", out)
	}

	// Empty output is a no-op, so a quiet run does not create a row that would
	// read as "recorded, but empty".
	if err := q.SetLog(ctx, id+"-nonexistent", ""); err != nil {
		t.Errorf("empty SetLog on an unknown job: %v", err)
	}
}

// Cancelling a queued job settles it without a worker ever seeing it.
func TestCancelQueuedJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewJob{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := q.Cancel(ctx, id)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if job.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", job.Status, StatusCancelled)
	}
	if !job.Terminal() {
		t.Error("a cancelled job does not report as terminal")
	}
	// It must not then be claimable: a cancelled job that a worker picks up
	// anyway is worse than one that never cancelled.
	if _, _, ok, err := q.claimNext(ctx); err != nil || ok {
		t.Errorf("claimNext after cancel: ok=%v err=%v", ok, err)
	}
}

// The case that matters: a job already executing stops, and is recorded as
// cancelled rather than as a failure.
func TestCancelRunningJob(t *testing.T) {
	q := testQueue(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	q.StartListener(ctx)

	started := make(chan struct{})
	q.StartWorkers(ctx, 1, func(runCtx context.Context, job Job, input []byte) (Outcome, error) {
		close(started)
		// Block until cancelled, as a long download would.
		<-runCtx.Done()
		return Outcome{}, runCtx.Err()
	})

	id, err := q.Enqueue(ctx, NewJob{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never started")
	}

	if _, err := q.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var job Job
	for time.Now().Before(deadline) {
		job, _, _ = q.Get(ctx, id)
		if job.Terminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if job.Status != StatusCancelled {
		t.Fatalf("status = %q, want %q (error=%q)", job.Status, StatusCancelled, job.Error)
	}
	// The run reported a context error on its way down, but the reason it went
	// down is known — recording it as a failure would misattribute a decision.
	if job.Error != "cancelled" {
		t.Errorf("error = %q, want %q", job.Error, "cancelled")
	}
}

// Cancelling something already finished is not an error: the caller wanted it
// stopped, and it is stopped.
func TestCancelFinishedJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewJob{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.pool.Exec(ctx,
		`UPDATE job SET status=$1, finished_at=$2 WHERE id=$3`, StatusDone, 1, id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Cancel(ctx, id); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("Cancel on a finished job = %v, want ErrNotCancellable", err)
	}
	if _, err := q.Cancel(ctx, "no-such-id"); !errors.Is(err, ErrNoSuchJob) {
		t.Errorf("Cancel on an unknown id = %v, want ErrNoSuchJob", err)
	}
}
