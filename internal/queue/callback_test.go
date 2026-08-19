package queue

import (
	"context"
	"sync"
	"testing"
)

// recorder collects notifications the queue decides to send.
type recorder struct {
	mu   sync.Mutex
	sent []string // "jobID status url"
}

func (r *recorder) notifier() Notifier {
	return func(jobID, url, status string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sent = append(r.sent, jobID+" "+status+" "+url)
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func TestAFinishedJobNotifiesItsCallback(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	rec := &recorder{}
	q.SetNotifier(rec.notifier())
	ctx := context.Background()

	id, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", Body: []byte("chr1:1:A:T"),
		CallbackURL: "https://hooks.example.org/vh",
	})
	if err != nil {
		t.Fatal(err)
	}

	c, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if rec.count() != 0 {
		t.Fatal("a running job notified; only a terminal one should")
	}
	q.finish(ctx, c, StatusDone, "", Outcome{})

	if rec.count() != 1 {
		t.Fatalf("sent %d notifications, want 1: %v", rec.count(), rec.sent)
	}
	if got := rec.sent[0]; got != id+" done https://hooks.example.org/vh" {
		t.Errorf("notification = %q", got)
	}
}

// A job nobody asked to be told about must not produce one.
func TestAJobWithNoCallbackNotifiesNobody(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	rec := &recorder{}
	q.SetNotifier(rec.notifier())
	ctx := context.Background()

	if _, err := q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := q.claimNext(ctx)
	q.finish(ctx, c, StatusDone, "", Outcome{})

	if rec.count() != 0 {
		t.Errorf("sent %v for a job that asked for nothing", rec.sent)
	}
}

// A failure is a terminal status and worth telling somebody about — arguably
// the one they most want to hear.
func TestAFailedJobNotifiesWithItsStatus(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	rec := &recorder{}
	q.SetNotifier(rec.notifier())
	ctx := context.Background()

	id, _ := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", Body: []byte("a"),
		CallbackURL: "https://hooks.example.org/vh",
	})
	c, _, _, _ := q.claimNext(ctx)
	q.finish(ctx, c, StatusError, "the reference was missing", Outcome{})

	if rec.count() != 1 {
		t.Fatalf("sent %d, want 1", rec.count())
	}
	if got := rec.sent[0]; got != id+" error https://hooks.example.org/vh" {
		t.Errorf("notification = %q, want the error status", got)
	}
}

// The reason callback_at exists. A fan-out job becomes terminal when its last
// chunk closes, and every worker finishing at that moment can observe it — so
// "is the job done?" is not safe to act on. Exactly one must win the right to
// say so, or a twenty-six-piece job notifies up to twenty-six times.
//
// Driven at notifyIfDone rather than through finish(), and that is not a
// shortcut. A locus submission has one chunk, so finishing "all of them"
// concurrently is one goroutine and the race never happens — the first version
// of this test passed without exercising anything. What is under test is the
// claim, so the claim is what gets hammered.
func TestOnlyOneWorkerWinsTheRightToNotify(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	rec := &recorder{}
	q.SetNotifier(rec.notifier())
	ctx := context.Background()

	id, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", Body: []byte("a"),
		CallbackURL: "https://hooks.example.org/vh",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	q.finish(ctx, c, StatusDone, "", Outcome{})
	if rec.count() != 1 {
		t.Fatalf("the finish itself sent %d, want 1", rec.count())
	}

	// Now every other worker that saw the same job settle, at once.
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.notifyIfDone(ctx, id)
		}()
	}
	wg.Wait()

	if n := rec.count(); n != 1 {
		t.Errorf("job %s notified %d times, want exactly 1: %v", id, n, rec.sent)
	}
}

// Firing must not depend on the notifier being fast, or a slow receiver would
// hold up the worker that finished the chunk. The queue hands off; it does not
// wait for delivery.
func TestTheQueueDoesNotWaitForDelivery(t *testing.T) {
	q := testQueue(t)
	q.nowFn = monotonicNow()
	release := make(chan struct{})
	done := make(chan struct{})
	q.SetNotifier(func(jobID, url, status string) {
		go func() {
			<-release
			close(done)
		}()
	})
	ctx := context.Background()

	if _, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", Body: []byte("a"),
		CallbackURL: "https://hooks.example.org/vh",
	}); err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := q.claimNext(ctx)
	q.finish(ctx, c, StatusDone, "", Outcome{}) // must return without delivery
	close(release)
	<-done
}
