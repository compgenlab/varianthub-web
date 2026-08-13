package queue

import (
	"context"
	"log"
	"time"
)

// StartListener runs a goroutine holding one dedicated connection that LISTENs
// on the queue's notification channels, until ctx is cancelled. It is what
// picks a queued chunk up promptly and what lets a cancellation reach the
// replica actually running the chunk.
//
// The worker calls this; the API server has no use for it, having nothing to be
// woken about. Everything still works without it — wake-ups fall back to a 1s
// ticker — just less promptly.
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

	for _, ch := range []string{chanQueued, chanCancel} {
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
		case chanCancel:
			q.stopLocal(n.Payload)
		}
	}
}

// stopLocal cancels a chunk this process is running.
//
// A cancel for a chunk running in another replica finds nothing here and is
// ignored, which is correct: every replica listens, so the one holding it will
// act. A cancel for a chunk that has already finished likewise does nothing.
func (q *Queue) stopLocal(id string) {
	q.mu.Lock()
	rj := q.running[id]
	if rj != nil {
		// Recorded before cancelling, so the worker sees the flag rather than
		// racing to interpret a context error it cannot attribute.
		rj.cancelled = true
	}
	q.mu.Unlock()
	if rj == nil {
		return
	}
	log.Printf("queue: cancelling chunk %s", id)
	rj.cancel()
}
