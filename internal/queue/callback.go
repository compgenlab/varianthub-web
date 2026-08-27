package queue

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5"
)

// Notifying a caller's server that a job has finished.
//
// The queue decides *when* and *whether*; it does not know how to make an HTTP
// request and should not. Delivery is a hook the process that owns the network
// sets — see internal/callback — so this package keeps no opinion about
// redirects, retries or which addresses may be reached.

// Notifier is told that a job has reached a terminal status.
//
// Called at most once per job. Whatever it does must not block: it is invoked
// from the path that finishes a chunk, and a worker waiting on somebody else's
// web server is a worker not annotating anything.
type Notifier func(jobID, url, status string)

// SetNotifier installs the delivery hook. Nil disables callbacks.
func (q *Queue) SetNotifier(n Notifier) { q.notify = n }

// claimCallback takes the right to notify for a job, if there is one owed.
//
// The conditional UPDATE is the whole mechanism. A fan-out job becomes terminal
// when its last chunk closes, and every one of twenty-six workers finishing in
// the same instant can observe that — so "is the job done?" is not a safe
// question to act on. "Did I win the right to say so?" is, and exactly one
// caller gets a row back.
//
// Reading the status through job_state in the same statement matters too: by
// the time this runs the chunks have settled, so the status returned is the
// job's real outcome rather than the one chunk's that happened to finish last.
func (q *Queue) claimCallback(ctx context.Context, jobID string) (url, status string, ok bool) {
	err := q.pool.QueryRow(ctx, `
		UPDATE job j SET callback_at = $2
		  FROM job_state s
		 WHERE s.id = j.id AND j.id = $1
		   AND j.callback_url IS NOT NULL AND j.callback_at IS NULL
		   AND s.finished_at IS NOT NULL
		RETURNING j.callback_url, s.status`, jobID, q.nowFn()).Scan(&url, &status)
	switch {
	case err == pgx.ErrNoRows:
		// The ordinary answer: no callback was asked for, the job is not
		// finished, or another worker got there first.
		return "", "", false
	case err != nil:
		log.Printf("queue: claim callback for job %s: %v", jobID, err)
		return "", "", false
	}
	return url, status, true
}

// notifyIfDone fires a job's callback if this is the moment it became terminal.
//
// Called after a chunk's outcome is committed, and never inside that
// transaction: a notification is about a fact, so it must not be sent for a
// finish that is later rolled back, and an HTTP request has no business holding
// a database transaction open.
func (q *Queue) notifyIfDone(ctx context.Context, jobID string) {
	if q.notify == nil || jobID == "" {
		return
	}
	url, status, ok := q.claimCallback(ctx, jobID)
	if !ok {
		return
	}
	q.notify(jobID, url, status)
}
