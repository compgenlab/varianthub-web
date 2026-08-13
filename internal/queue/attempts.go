package queue

import "context"

// Attempt is one time a job was handed to a worker.
type Attempt struct {
	// N matches job.attempts as it stood when this attempt was claimed.
	N int `json:"attempt"`
	// Worker is the process that claimed it. Omitted for callers who should not
	// see the deployment's internals; see api.attemptsFor.
	Worker string `json:"worker,omitempty"`
	// StartedAt is when it was claimed; EndedAt is 0 while it is still in flight.
	StartedAt int64 `json:"started_at"`
	EndedAt   int64 `json:"ended_at,omitempty"`
	// Outcome is "" while running, otherwise done/error/cancelled/abandoned.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Abandoned reports whether this attempt's worker disappeared rather than
// reporting.
func (a Attempt) Abandoned() bool { return a.Outcome == OutcomeAbandoned }

// Duration is how long the attempt lasted, in seconds, or 0 while it is still
// running.
//
// For an abandoned attempt this is how long it survived before its worker went
// away — which is the part that separates "killed under load partway through"
// from "died on startup", two very different problems that job.attempts reports
// with the same number.
func (a Attempt) Duration() int64 {
	if a.EndedAt == 0 {
		return 0
	}
	return a.EndedAt - a.StartedAt
}

// JobAttempts returns a job's attempts, oldest first.
func (q *Queue) JobAttempts(ctx context.Context, id string) ([]Attempt, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT attempt, worker, started_at, COALESCE(ended_at,0),
		       COALESCE(outcome,''), COALESCE(error,'')
		  FROM job_attempt WHERE job_id = $1 ORDER BY attempt`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.N, &a.Worker, &a.StartedAt, &a.EndedAt,
			&a.Outcome, &a.Error); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// WorkerHealth is one worker's recent record.
type WorkerHealth struct {
	Worker string `json:"worker"`
	// Attempts it started in the window; Abandoned is how many of those it did
	// not come back from.
	Attempts  int64 `json:"attempts"`
	Abandoned int64 `json:"abandoned"`
	// MedianAbandonedAfter is the typical seconds an abandoned attempt survived,
	// 0 when none were. A worker killed partway through long jobs and one that
	// dies immediately on startup both show an abandonment count; this is what
	// tells them apart, and the memory limit is the usual reason for the first.
	MedianAbandonedAfter int64 `json:"median_abandoned_after,omitempty"`
}

// WorkerHealthSince summarises each worker's attempts since a Unix timestamp,
// worst abandonment count first.
//
// This is the question job_attempt exists to answer. job.attempts is a counter
// with no dimensions: three abandonments could be one pod losing all of them —
// a bad node, a memory limit set too low for that pod — or three pods each
// losing one, which points at the jobs instead. The counter shows 3 either way,
// and before this the only way to tell was grepping individual job logs for the
// line naming the worker.
func (q *Queue) WorkerHealthSince(ctx context.Context, since int64) ([]WorkerHealth, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT worker,
		       count(*),
		       count(*) FILTER (WHERE outcome = $1),
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY ended_at - started_at)
		         FILTER (WHERE outcome = $1 AND ended_at IS NOT NULL), 0)
		  FROM job_attempt
		 WHERE started_at >= $2
		 GROUP BY worker
		 ORDER BY count(*) FILTER (WHERE outcome = $1) DESC, worker`,
		OutcomeAbandoned, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerHealth
	for rows.Next() {
		var h WorkerHealth
		// percentile_cont returns double precision; take it as a float and round,
		// since a median of "half a second" is not a distinction anyone needs.
		var median float64
		if err := rows.Scan(&h.Worker, &h.Attempts, &h.Abandoned, &median); err != nil {
			return nil, err
		}
		h.MedianAbandonedAfter = int64(median + 0.5)
		out = append(out, h)
	}
	return out, rows.Err()
}
