package queue

import (
	"context"
	"log"
	"time"
)

// StartListener runs a goroutine holding one dedicated connection that LISTENs on
// the queue's notification channels, until ctx is cancelled. It is what makes
// WaitFor cheap and what lets a waiter in one replica learn about a job finished
// by a worker in another.
//
// Both the API server and the worker process should call this. Everything still
// works without it — worker wake-ups fall back to a 1s ticker and WaitFor to its
// safety poll — just less promptly.
func (q *Queue) StartListener(ctx context.Context) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for {
			if ctx.Err() != nil {
				return
			}
			if err := q.listen(ctx); err != nil && ctx.Err() == nil {
				log.Printf("queue: listener: %v (reconnecting)", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	}()
}

// listen holds a connection and dispatches notifications until it errors.
func (q *Queue) listen(ctx context.Context) error {
	conn, err := q.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	for _, ch := range []string{chanQueued, chanDone} {
		// Channel names are compile-time constants, not user input.
		if _, err := conn.Exec(ctx, `LISTEN `+ch); err != nil {
			return err
		}
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		switch n.Channel {
		case chanQueued:
			q.poke()
		case chanDone:
			q.wake(n.Payload)
		}
	}
}

// wake releases everyone blocked in WaitFor for job id.
func (q *Queue) wake(id string) {
	q.mu.Lock()
	chans := q.waiters[id]
	delete(q.waiters, id)
	q.mu.Unlock()
	for _, c := range chans {
		close(c)
	}
}

// subscribe registers a channel closed when job id reaches a terminal status.
// The returned func removes the registration and must always be called.
func (q *Queue) subscribe(id string) (<-chan struct{}, func()) {
	c := make(chan struct{})
	q.mu.Lock()
	q.waiters[id] = append(q.waiters[id], c)
	q.mu.Unlock()

	return c, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		rest := q.waiters[id][:0]
		for _, x := range q.waiters[id] {
			if x != c {
				rest = append(rest, x)
			}
		}
		if len(rest) == 0 {
			delete(q.waiters, id)
		} else {
			q.waiters[id] = rest
		}
	}
}

// waitPoll bounds how long WaitFor can sleep without re-checking the row. A
// NOTIFY normally wakes it far sooner; this only covers a dropped notification
// or a process with no listener running.
const waitPoll = 2 * time.Second

// WaitFor blocks until job id reaches a terminal status (done/error), the timeout
// elapses, or ctx is cancelled — returning the latest job seen. ok=false only when
// the id is unknown. A timeout <= 0 returns the current job immediately.
//
// This holds an HTTP goroutine, never a worker: the annotation runs in the pool.
func (q *Queue) WaitFor(ctx context.Context, id string, timeout time.Duration) (Job, bool, error) {
	job, ok, err := q.Get(ctx, id)
	if err != nil || !ok || timeout <= 0 || job.Terminal() {
		return job, ok, err
	}

	// Subscribe before the re-read below, so a job finishing in between is not
	// missed: the notification would then find a registered waiter.
	done, unsubscribe := q.subscribe(id)
	defer unsubscribe()

	if job, ok, err = q.Get(ctx, id); err != nil || !ok || job.Terminal() {
		return job, ok, err
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(waitPoll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return job, true, ctx.Err()
		case <-deadline.C:
			job, ok, err = q.Get(ctx, id)
			return job, ok, err
		case <-done:
			job, ok, err = q.Get(ctx, id)
			return job, ok, err
		case <-ticker.C:
			job, ok, err = q.Get(ctx, id)
			if err != nil || !ok || job.Terminal() {
				return job, ok, err
			}
		}
	}
}
