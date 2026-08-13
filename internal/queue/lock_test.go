package queue

import (
	"context"
	"testing"
)

// Two processes, one lock: the second is told no rather than made to wait.
//
// The work behind this lock is a full listing of job storage. A replica that
// waited would do the same listing a moment later for nothing; being refused is
// the answer it wants.
func TestOnlyOneHolderGetsTheLock(t *testing.T) {
	a, b := testQueuePair(t)
	ctx := context.Background()

	got, release, err := a.TryLock(ctx, "sweep-test")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("the first caller did not get an uncontended lock")
	}

	blocked, releaseB, err := b.TryLock(ctx, "sweep-test")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		releaseB()
		t.Fatal("two holders at once; the sweep would run twice")
	}

	// Released, the next caller gets it — otherwise one crashed sweep would
	// stop every later one.
	release()
	again, releaseC, err := b.TryLock(ctx, "sweep-test")
	if err != nil {
		t.Fatal(err)
	}
	if !again {
		t.Fatal("the lock was not released")
	}
	releaseC()
}

// Different names do not contend, or one sweep would block an unrelated one.
func TestDifferentLockNamesDoNotContend(t *testing.T) {
	a, b := testQueuePair(t)
	ctx := context.Background()

	gotA, releaseA, err := a.TryLock(ctx, "one")
	if err != nil || !gotA {
		t.Fatalf("first lock: %v got=%v", err, gotA)
	}
	defer releaseA()

	gotB, releaseB, err := b.TryLock(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	if !gotB {
		t.Fatal("an unrelated lock name was blocked")
	}
	releaseB()
}

// Every job, whatever state it is in.
//
// The sweep deletes what is *absent* from this set, so a filter here becomes a
// deletion there: narrowing it to finished jobs would collect the input of
// everything currently queued.
//
// Jobs rather than chunks because storage is laid out by job — everything a
// submission owns lives under jobs/<job-id>/ — so a set of chunk ids would
// match no prefix at all and the sweep would delete every one of them.
func TestKnownJobIDsCoversEveryState(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	var jobs []string
	for i := 0; i < 3; i++ {
		jobs = append(jobs, submitOne(t, q, "u"))
	}

	// One left running, one finished, one still queued. Which chunk the queue
	// hands back is its business — what matters is that all three states are
	// represented.
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	done, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	q.finish(ctx, done, StatusDone, "", Outcome{})

	ids, err := q.KnownJobIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range jobs {
		if !ids[id] {
			t.Errorf("job %s is missing; the sweep would delete its files", id)
		}
	}
}
