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
			INSERT INTO chunk (id,kind,snapshot,selection,status,n_variants,created_at,finished_at)
			VALUES ($1,'locus','snap','',$2,$3,$4,$5)`,
			id, status, variants, created, fin); err != nil {
			t.Fatal(err)
		}
	}

	insert("d1", StatusDone, 10, now-3600, now-3000)           // within 24h
	insert("d2", StatusDone, 32, now-3*24*3600, now-3*24*3600) // within 7d, not 24h
	insert("d3", StatusDone, 5, now-30*24*3600, now-30*24*3600)
	// A failed chunk's n_variants is what was submitted, not what was
	// annotated.
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
	// 10+32+5 — the failed chunk's 999 must not be counted as annotated.
	if s.Variants != 47 {
		t.Errorf("variants = %d, want 47 (successful chunks only)", s.Variants)
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
func TestChunkLog(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewChunk{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing recorded yet is distinguishable from a run that printed nothing.
	out, found, err := q.Log(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if found || out != "" {
		t.Errorf("a fresh chunk reports a log: %q (found=%v)", out, found)
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
	// recorded on the chunk, and two runs' output interleaved describes
	// neither.
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
		t.Errorf("empty SetLog on an unknown chunk: %v", err)
	}
}

// Cancelling a queued chunk settles it without a worker ever seeing it.
func TestCancelQueuedChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewChunk{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := q.Cancel(ctx, id)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if chunk.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", chunk.Status, StatusCancelled)
	}
	if !chunk.Terminal() {
		t.Error("a cancelled chunk does not report as terminal")
	}
	// It must not then be claimable: a cancelled chunk that a worker picks up
	// anyway is worse than one that never cancelled.
	if _, _, ok, err := q.claimNext(ctx); err != nil || ok {
		t.Errorf("claimNext after cancel: ok=%v err=%v", ok, err)
	}
}

// The case that matters: a chunk already executing stops, and is recorded as
// cancelled rather than as a failure.
func TestCancelRunningChunk(t *testing.T) {
	q := testQueue(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	q.StartListener(ctx)

	started := make(chan struct{})
	q.StartWorkers(ctx, 1, func(runCtx context.Context, chunk Chunk, input []byte) (Outcome, error) {
		close(started)
		// Block until cancelled, as a long download would.
		<-runCtx.Done()
		return Outcome{}, runCtx.Err()
	})

	id, err := q.Enqueue(ctx, NewChunk{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the chunk never started")
	}

	if _, err := q.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var chunk Chunk
	for time.Now().Before(deadline) {
		chunk, _, _ = q.Get(ctx, id)
		if chunk.Terminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if chunk.Status != StatusCancelled {
		t.Fatalf("status = %q, want %q (error=%q)", chunk.Status, StatusCancelled, chunk.Error)
	}
	// The run reported a context error on its way down, but the reason it went
	// down is known — recording it as a failure would misattribute a decision.
	if chunk.Error != "cancelled" {
		t.Errorf("error = %q, want %q", chunk.Error, "cancelled")
	}
}

// Cancelling something already finished is not an error: the caller wanted it
// stopped, and it is stopped.
func TestCancelFinishedChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewChunk{Kind: KindDownload, Snapshot: "s", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.pool.Exec(ctx,
		`UPDATE chunk SET status=$1, finished_at=$2 WHERE id=$3`, StatusDone, 1, id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Cancel(ctx, id); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("Cancel on a finished chunk = %v, want ErrNotCancellable", err)
	}
	if _, err := q.Cancel(ctx, "no-such-id"); !errors.Is(err, ErrNoSuchChunk) {
		t.Errorf("Cancel on an unknown id = %v, want ErrNoSuchChunk", err)
	}
}

// The pool is a budget, not a headcount. A heavy chunk holds more of it, so
// two large provisioning runs do not overlap — they finish later that way and
// gain nothing, and while they overlap there is nothing left for annotation.
func TestChunkWeightLimitsConcurrency(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	q.SetSlots(2)

	heavy := func() string {
		t.Helper()
		id, err := q.Enqueue(ctx, NewChunk{
			Kind: KindDownload, Snapshot: "s", Weight: 2, Body: []byte("{}"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	light := func() string {
		t.Helper()
		id, err := q.Enqueue(ctx, NewChunk{Kind: KindLocus, Snapshot: "s", Body: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	first, second := heavy(), heavy()

	// The first fills the budget on its own.
	got, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	// Which of the two goes first is not asserted: they were created in the
	// same second, and the ordering then falls to a random id — a real property
	// of the queue rather than something this test should pin.
	if got.ID != first && got.ID != second {
		t.Fatalf("claimed %s, which is neither queued chunk", got.ID)
	}
	if got.Weight != 2 {
		t.Fatalf("claimed weight %d, want 2", got.Weight)
	}
	running := got.ID

	// The second cannot start, and neither can a light chunk — there is no
	// room.
	if _, _, ok, err = q.claimNext(ctx); err != nil || ok {
		t.Errorf("a second heavy chunk was claimed with no slots free: ok=%v err=%v", ok, err)
	}
	lightID := light()
	if _, _, ok, err = q.claimNext(ctx); err != nil || ok {
		t.Errorf("a light chunk was claimed with no slots free: ok=%v err=%v", ok, err)
	}

	// When the heavy one finishes, the budget frees up. Which of the two
	// waiting chunks goes first is not asserted: they were created in the same
	// second, and the ordering then falls to a random id — a real property of
	// the queue, not something this test should pin.
	q.finish(ctx, running, StatusDone, "", Outcome{})
	got, _, ok, err = q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim after the first finished: ok=%v err=%v", ok, err)
	}
	if got.ID == running || (got.ID != first && got.ID != second && got.ID != lightID) {
		t.Fatalf("claimed %s, which is not one of the waiting chunks", got.ID)
	}
	// The budget still holds: if the light one went first, the heavy one has to
	// keep waiting, because 1 free slot cannot take a weight of 2.
	if got.ID == lightID {
		if _, _, ok, _ := q.claimNext(ctx); ok {
			t.Error("a weight-2 chunk was claimed with only one slot free")
		}
	}
}

// A deployment with room runs light chunks alongside each other, which is the
// behaviour a weight of 1 has to keep.
func TestLightChunksStillShareThePool(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	q.SetSlots(2)

	for i := 0; i < 2; i++ {
		if _, err := q.Enqueue(ctx, NewChunk{
			Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0." + string(rune('1'+i)),
			Body: []byte("{}"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
	}
	// ...and the third waits, because the budget is spent.
	if _, err := q.Enqueue(ctx, NewChunk{Kind: KindLocus, Snapshot: "s", Body: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := q.claimNext(ctx); ok {
		t.Error("a third chunk was claimed with both slots in use")
	}
}

// A chunk heavier than the whole pool must still run — alone.
//
// Otherwise it is unclaimable: the predicate asks whether the running total
// plus this chunk fits, and on an idle single-slot pool 0+2 <= 1 is false, so
// a weight-2 download sits queued forever behind nothing at all. There is no
// error and no log line; the queue simply never picks it up, which reads as a
// hung worker rather than a budget that cannot express the chunk.
//
// VHW_WORKERS=1 is an ordinary small deployment, and JobSlots follows Workers,
// so this is the default single-worker configuration rather than a corner.
func TestAChunkHeavierThanThePoolStillRuns(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	q.SetSlots(1) // one worker; a download weighs 2

	id, err := q.Enqueue(ctx, NewChunk{
		Kind: KindDownload, Snapshot: "s", Weight: 2, Body: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, _, ok, err := q.claimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("weight-2 chunk never claimed on a 1-slot pool: it would queue forever")
	}
	if got.ID != id {
		t.Fatalf("claimed %s, want %s", got.ID, id)
	}

	// It still holds the pool exclusively: nothing else may join it.
	if _, err = q.Enqueue(ctx, NewChunk{Kind: KindLocus, Snapshot: "s", Body: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err = q.claimNext(ctx); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("a second chunk joined an already-oversubscribed pool")
	}
}
