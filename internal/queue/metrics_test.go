package queue

import (
	"context"
	"testing"
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
