package queue

import (
	"context"
	"strings"
	"testing"
)

// expire makes a running job's lease look lapsed, which is what a killed worker
// leaves behind: the row says running, and nothing is renewing it.
func expire(t *testing.T, q *Queue, id string) {
	t.Helper()
	if _, err := q.pool.Exec(context.Background(),
		`UPDATE job SET lease_until = 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// An abandoned job's own log has to say which worker dropped it. The reclaim
// clears claimed_by, so read after the fact the identity is simply gone — and
// "abandoned 3 times" with no worker named cannot be turned into "worker-7 is
// losing all of them", which is the question when a pod is being OOM-killed.
func TestAnAbandonedJobRecordsWhichWorkerLostIt(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	expire(t, q, id)

	if _, err := q.ReclaimExpired(ctx); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	logText, _, err := q.Log(ctx, id)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if !strings.Contains(logText, "stopped renewing the lease") {
		t.Fatalf("the abandonment is not in the job's log: %q", logText)
	}
	// The worker that claimed it, by name — not the generic phrasing used only
	// when claimed_by was somehow empty.
	if !strings.Contains(logText, q.WorkerID()) {
		t.Errorf("the log does not name the worker that lost it: %q", logText)
	}
}

// Abandonment is counted apart from failure. A failure is the job going wrong; an
// abandonment is the process running it disappearing, and folding them together
// would hide a deployment losing capacity inside an ordinary error rate.
func TestStatsCountAbandonmentApartFromFailure(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := q.claimNext(ctx); !ok {
		t.Fatal("claim")
	}
	expire(t, q, id)
	if _, err := q.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}

	st, err := q.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Requeued after one lost attempt: retrying, not yet given up, and not a
	// failure.
	if st.AbandonedRetrying != 1 {
		t.Errorf("abandoned_retrying = %d, want 1", st.AbandonedRetrying)
	}
	if st.AbandonedExhausted != 0 {
		t.Errorf("abandoned_exhausted = %d, want 0 — it still has attempts left",
			st.AbandonedExhausted)
	}
	if st.Failed != 0 {
		t.Errorf("failed = %d; an abandonment is not a failure", st.Failed)
	}

	// Burn the remaining attempts; it should end up exhausted rather than
	// retrying.
	for i := 0; i < MaxAttempts; i++ {
		if _, _, ok, _ := q.claimNext(ctx); ok {
			expire(t, q, id)
		}
		if _, err := q.ReclaimExpired(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st, err = q.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.AbandonedExhausted != 1 {
		t.Errorf("abandoned_exhausted = %d, want 1 after %d attempts",
			st.AbandonedExhausted, MaxAttempts)
	}
	if st.AbandonedRetrying != 0 {
		t.Errorf("abandoned_retrying = %d, want 0 once it has given up",
			st.AbandonedRetrying)
	}
}
