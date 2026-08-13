package queue

import (
	"context"
	"testing"
)

// claimIDs claims up to n chunks and reports whose they were, finishing each
// so the next claim sees the completion — which is the whole point: the
// fairness signal that matters at one slot only exists once something has
// finished.
func claimIDs(t *testing.T, q *Queue, n int) []string {
	t.Helper()
	ctx := context.Background()
	var who []string
	for i := 0; i < n; i++ {
		chunk, _, ok, err := q.claimNext(ctx)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if !ok {
			break
		}
		who = append(who, chunk.UserID)
		q.finish(ctx, chunk, StatusDone, "", Outcome{})
	}
	return who
}

// fairQueue is a queue with a clock that actually advances. nowFn is Unix
// *seconds* in production, and these tests run in milliseconds — so without this
// every created_at and last_finished_at ties, the ordering falls through to
// created_at, and the test would measure the resolution of the clock rather than
// the fairness of the scheduler.
func fairQueue(t *testing.T) *Queue {
	t.Helper()
	q := testQueue(t)
	q.nowFn = monotonicNow()
	q.SetSlots(1)
	return q
}

func enqueueFor(t *testing.T, q *Queue, user string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := q.Submit(ctx, NewJob{
			Kind: KindLocus, Snapshot: "s", UserID: user, Body: []byte("chr1:1:A:T"),
		}); err != nil {
			t.Fatalf("enqueue for %s: %v", user, err)
		}
	}
}

// The bug this exists for. With one slot the running-count term is a constant
// — by the time a worker claims, the chunk it just finished is already done,
// so every caller has zero running — and the ordering used to collapse to
// plain FIFO. A caller who queued a job before anyone else arrived took all of
// it in a row.
//
// One slot is not a corner: VHW_WORKERS=1 is an ordinary deployment, and slots
// follow workers.
func TestOneSlotDoesNotStarveTheSecondCaller(t *testing.T) {
	q := fairQueue(t)

	// "big" gets in first with a job; "small" arrives after with one chunk.
	enqueueFor(t, q, "big", 8)
	enqueueFor(t, q, "small", 1)

	got := claimIDs(t, q, 5)
	if len(got) < 5 {
		t.Fatalf("claimed %d chunks, want 5: %v", len(got), got)
	}
	// small must be served within the first few, not after all eight.
	served := -1
	for i, w := range got {
		if w == "small" {
			served = i
			break
		}
	}
	if served < 0 {
		t.Fatalf("the second caller was never served in 5 claims: %v", got)
	}
	if served > 2 {
		t.Errorf("the second caller waited %d chunks (%v); the job is monopolizing", served, got)
	}
}

// Once both are being served, neither should run two in a row while the other
// waits — that is the round-robin the effective time buys.
func TestOneSlotAlternatesBetweenCallers(t *testing.T) {
	q := fairQueue(t)

	enqueueFor(t, q, "a", 6)
	enqueueFor(t, q, "b", 6)

	got := claimIDs(t, q, 8)
	if len(got) < 8 {
		t.Fatalf("claimed %d, want 8: %v", len(got), got)
	}
	// Count the longest run by one caller. Perfect alternation gives 1; the old
	// FIFO ordering gives 6.
	longest, run := 1, 1
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
	}
	if longest > 2 {
		t.Errorf("one caller ran %d in a row: %v", longest, got)
	}
}

// A caller who has never run is ordered by their submit time, which is "now" —
// so they join the rotation rather than preempting it. This is what the
// wall-clock timestamp buys over an accumulating counter, where a newcomer
// seeded at zero would jump the whole queue.
func TestANewcomerJoinsTheRotationRatherThanPreemptingIt(t *testing.T) {
	q := fairQueue(t)
	ctx := context.Background()

	enqueueFor(t, q, "a", 4)
	// Let "a" get established, so it has a last-finished timestamp.
	claimIDs(t, q, 2)

	enqueueFor(t, q, "newcomer", 2)
	got := claimIDs(t, q, 4)

	var n int
	for _, w := range got {
		if w == "newcomer" {
			n++
		}
	}
	// Present, but not taking everything: 2 of its own among 4 claims is the
	// most it should get, and at least 1 means it is not starved.
	if n == 0 {
		t.Errorf("the newcomer was starved: %v", got)
	}
	if n > 2 {
		t.Errorf("the newcomer preempted the queue (%d of 4): %v", n, got)
	}
	_ = ctx
}

// The timestamp is charged to the same identity the ordering reads. Written
// twice — once in Go, once in SQL — the two could disagree, and a chunk
// charged to one identity while ordered under another is a scheduler that
// quietly stops being fair.
func TestFinishingChargesTheCallerTheOrderingReads(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u1", ClientIP: "10.0.0.1",
		Body: []byte("chr1:1:A:T"),
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	q.finish(ctx, chunk, StatusDone, "", Outcome{})

	var who string
	var at int64
	if err := q.pool.QueryRow(ctx,
		`SELECT who, last_finished_at FROM queue_caller`).Scan(&who, &at); err != nil {
		t.Fatalf("no fair-share row was written: %v", err)
	}
	// The account, not the address: a signed-in caller is the same person from
	// two machines, and two people behind one NAT are not the same caller.
	if who != "u1" {
		t.Errorf("charged %q, want the account", who)
	}
	if at == 0 {
		t.Errorf("last_finished_at not set")
	}
	_ = id
}

// An anonymous caller is charged by address, since that is all there is to be
// fair between.
func TestAnAnonymousCallerIsChargedByAddress(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	if _, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.2", Body: []byte("chr1:1:A:T"),
	}); err != nil {
		t.Fatal(err)
	}
	chunk, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	q.finish(ctx, chunk, StatusDone, "", Outcome{})

	var who string
	if err := q.pool.QueryRow(ctx, `SELECT who FROM queue_caller`).Scan(&who); err != nil {
		t.Fatalf("no row: %v", err)
	}
	if who != "10.0.0.2" {
		t.Errorf("charged %q, want the address", who)
	}
}

// The timestamps age out with the chunks. Safe because the ordering reads
// GREATEST(created_at, last_finished_at): a row older than any queued chunk
// never wins that comparison, so dropping it changes no decision.
func TestSweepingChunksAlsoPrunesTheFairShareRows(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	if _, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "old", Body: []byte("chr1:1:A:T"),
	}); err != nil {
		t.Fatal(err)
	}
	chunk, _, _, _ := q.claimNext(ctx)
	q.finish(ctx, chunk, StatusDone, "", Outcome{})

	var n int
	if err := q.pool.QueryRow(ctx, `SELECT COUNT(*) FROM queue_caller`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected one fair-share row, got %d", n)
	}

	// A cutoff after everything: the chunk goes, and so does its timestamp.
	if _, err := q.DeleteOlderThan(ctx, 1<<62); err != nil {
		t.Fatal(err)
	}
	if err := q.pool.QueryRow(ctx, `SELECT COUNT(*) FROM queue_caller`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d fair-share row(s) outlived the chunks they describe", n)
	}
}
