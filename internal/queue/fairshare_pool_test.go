package queue

import (
	"context"
	"testing"
)

// Fairness with more than one slot.
//
// Every other test in fairshare_test.go runs a one-slot pool, because one slot
// is where the original bug lived: the running-count term is a constant there,
// so the ordering collapsed to FIFO and one caller's backlog took the queue.
// That left the ordinary deployment — VHW_JOB_SLOTS above 1, or the several
// workers it follows — asserted by nothing at all.
//
// The two terms trade places as the pool grows, which is why this needs its own
// coverage rather than a bigger number in the existing tests. At one slot the
// running count says nothing and effective time does the work; at four slots the
// running count is the term that stops a caller taking every slot, and it is the
// only one that can, because a caller whose chunks have not finished has no
// last_finished_at to order by.

// pool claims while there is room and finishes chunks oldest-first, which is a
// worker pool with uniform chunk durations. Returns who was served, in order.
func pool(t *testing.T, q *Queue, slots, rounds int) []string {
	t.Helper()
	ctx := context.Background()
	var running []Chunk
	var served []string
	for i := 0; i < rounds; i++ {
		for len(running) < slots {
			c, _, ok, err := q.claimNext(ctx)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if !ok {
				break
			}
			running = append(running, c)
			served = append(served, c.UserID)
		}
		if len(running) == 0 {
			break
		}
		q.finish(ctx, running[0], StatusDone, "", Outcome{})
		running = running[1:]
	}
	return served
}

func poolQueue(t *testing.T, slots int) *Queue {
	t.Helper()
	q := testQueue(t)
	// Unix seconds in production; these run in milliseconds, so without a clock
	// that advances every timestamp ties and this measures the clock rather than
	// the scheduler.
	q.nowFn = monotonicNow()
	q.SetSlots(slots)
	return q
}

// longestRun is how many times in a row one caller was served.
func longestRun(served []string) int {
	longest, run := 0, 0
	for i, w := range served {
		if i > 0 && w == served[i-1] {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
	}
	return longest
}

// Two callers with equal backlogs alternate, whatever the pool's size.
//
// The one-slot version of this is TestOneSlotAlternatesBetweenCallers. It is
// repeated across sizes because the term that produces the alternation is not
// the same one at every size, so a pass at one slot says nothing about four.
func TestAPoolAlternatesBetweenCallersAtEverySize(t *testing.T) {
	for _, slots := range []int{1, 2, 3, 4} {
		q := poolQueue(t, slots)
		enqueueFor(t, q, "a", 12)
		enqueueFor(t, q, "b", 12)

		served := pool(t, q, slots, 16)
		if len(served) < 12 {
			t.Fatalf("slots=%d: only %d chunks were served: %v", slots, len(served), served)
		}
		if run := longestRun(served); run > 2 {
			t.Errorf("slots=%d: one caller ran %d in a row: %v", slots, run, served)
		}
	}
}

// A caller arriving behind a backlog is served within about one pool's worth of
// claims, not after the backlog drains.
//
// The bound is slots+1 rather than a constant: with a pool of four, three chunks
// of somebody else's may legitimately already be in flight when the newcomer
// arrives. What must not happen is waiting for the twenty ahead of them.
func TestALatecomerIsServedWithinAPoolAtEverySize(t *testing.T) {
	for _, slots := range []int{1, 2, 4} {
		q := poolQueue(t, slots)
		enqueueFor(t, q, "backlog", 20)
		enqueueFor(t, q, "latecomer", 1)

		served := pool(t, q, slots, 12)
		at := -1
		for i, w := range served {
			if w == "latecomer" {
				at = i
				break
			}
		}
		if at < 0 {
			t.Fatalf("slots=%d: the latecomer was never served: %v", slots, served)
		}
		if at > slots {
			t.Errorf("slots=%d: the latecomer waited %d chunks, want at most %d: %v",
				slots, at, slots, served)
		}
	}
}

// At saturation no caller holds the whole pool.
//
// Nothing has finished here, so last_finished_at is unset for everyone and this
// rests entirely on the running count — the case that term exists for, and the
// one it is the only term able to see.
func TestSaturationSpreadsTheSlotsAcrossCallers(t *testing.T) {
	callers := []string{"a", "b", "c"}
	for _, slots := range []int{2, 3, 4, 6} {
		q := poolQueue(t, slots)
		for _, c := range callers {
			enqueueFor(t, q, c, 10)
		}
		ctx := context.Background()

		held := map[string]int{}
		for i := 0; i < slots; i++ {
			c, _, ok, err := q.claimNext(ctx)
			if err != nil {
				t.Fatalf("slots=%d claim: %v", slots, err)
			}
			if !ok {
				t.Fatalf("slots=%d: the pool did not fill; claimed %d", slots, i)
			}
			held[c.UserID]++
		}
		// An even spread, to the nearest whole slot. Three callers and four
		// slots is 2/1/1, and the caller with two is not a failure — there is
		// no fourth slot to give away.
		want := (slots + len(callers) - 1) / len(callers)
		for _, c := range callers {
			if held[c] > want {
				t.Errorf("slots=%d: caller %q holds %d of %d slots, want at most %d (held %v)",
					slots, c, held[c], slots, want, held)
			}
		}
	}
}

// A caller whose chunks are still running keeps sharing the pool.
//
// This is the interaction the two terms exist to cover between them. A caller
// with long chunks in flight never advances last_finished_at, so by that measure
// they look permanently idle and would win every tie; the running count is what
// holds them to their share. Take that term away and "stuck" takes every slot as
// the quick caller frees it.
func TestACallerWhoseChunksNeverFinishStillSharesThePool(t *testing.T) {
	const slots = 4
	q := poolQueue(t, slots)
	// Far more quick work than the run needs, so the pool is never idle for
	// want of something to give the quick caller — otherwise "stuck" fills the
	// slots legitimately and this measures the fixture, not the scheduler.
	enqueueFor(t, q, "stuck", 10)
	enqueueFor(t, q, "quick", 200)
	ctx := context.Background()

	var stuck, quick []Chunk
	quickServed := 0
	for i := 0; i < 24; i++ {
		for len(stuck)+len(quick) < slots {
			c, _, ok, err := q.claimNext(ctx)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if !ok {
				break
			}
			if c.UserID == "stuck" {
				stuck = append(stuck, c) // never finished, like a six-hour chunk
			} else {
				quick = append(quick, c)
				quickServed++
			}
		}
		if len(quick) == 0 {
			t.Fatalf("the quick caller holds no slot at round %d; stuck holds %d of %d",
				i, len(stuck), slots)
		}
		q.finish(ctx, quick[0], StatusDone, "", Outcome{})
		quick = quick[1:]
	}

	if len(stuck) > slots/2 {
		t.Errorf("the stuck caller holds %d of %d slots; long-running chunks are "+
			"crowding out a caller who is still finishing work", len(stuck), slots)
	}
	if quickServed < 12 {
		t.Errorf("the quick caller was served only %d times in 24 rounds", quickServed)
	}
}

// Workers claiming at once must not over-commit the pool.
//
// SKIP LOCKED does not lock the running set the capacity check reads, so two
// workers claiming in the same instant could both see room only one of them has.
// claimNext takes an advisory lock for exactly this, and this is what says so —
// at one slot it is also the only thing standing between a one-worker pool and
// running two chunks at once.
func TestConcurrentWorkersDoNotOvercommitThePool(t *testing.T) {
	for _, slots := range []int{1, 2, 4} {
		q := poolQueue(t, slots)
		q.SetMaxJobsPerIP(0)
		enqueueFor(t, q, "a", 40)
		ctx := context.Background()

		const workers = 24
		got := make(chan bool, workers)
		errs := make(chan error, workers)
		start := make(chan struct{})
		for i := 0; i < workers; i++ {
			go func() {
				<-start // maximize contention
				_, _, ok, err := q.claimNext(ctx)
				if err != nil {
					errs <- err
					return
				}
				got <- ok
			}()
		}
		close(start)

		claimed := 0
		for i := 0; i < workers; i++ {
			select {
			case err := <-errs:
				t.Fatalf("slots=%d concurrent claim: %v", slots, err)
			case ok := <-got:
				if ok {
					claimed++
				}
			}
		}
		if claimed != slots {
			t.Errorf("slots=%d: %d workers claimed %d chunks, want exactly %d",
				slots, workers, claimed, slots)
		}
	}
}
