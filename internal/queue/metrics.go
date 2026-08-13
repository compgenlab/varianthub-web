package queue

import (
	"context"
	"strings"
)

// Stats summarises the queue and the work it has done.
type Stats struct {
	// Lifetime counts, by terminal state.
	Total     int64 `json:"total"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	// Cancelled is counted apart from failed: someone stopping a chunk on
	// purpose is not the deployment going wrong, and folding the two together
	// would make the failure rate move for reasons nobody needs to chase.
	Cancelled int64 `json:"cancelled"`

	// What is in flight right now.
	Queued  int64 `json:"queued"`
	Running int64 `json:"running"`

	// OldestQueuedAt is the creation time of the longest-waiting queued chunk,
	// 0 when nothing is waiting. A depth alone does not distinguish a queue
	// that is moving from one that is stuck.
	OldestQueuedAt int64 `json:"oldest_queued_at,omitempty"`

	// Variants counts variants across chunks that finished successfully.
	// Failed chunks are excluded: their n_variants is what was submitted, not
	// what was annotated, and counting it would inflate the total exactly when
	// something is going wrong.
	Variants int64 `json:"variants"`

	// Recent activity, by completion time.
	Last24h int64 `json:"last_24h"`
	Last7d  int64 `json:"last_7d"`

	// Abandoned counts chunks whose worker was killed rather than reporting —
	// currently retrying, plus those that gave up. Counted apart from Failed
	// because it is a different kind of trouble: a failure is the chunk going
	// wrong, an abandonment is the process running it disappearing, and the
	// usual cause is the container's memory limit.
	//
	// Retrying and exhausted are separate because they answer different
	// questions. A steady trickle of retries that eventually succeed is a
	// deployment losing capacity; exhausted chunks are work that was thrown
	// away.
	AbandonedRetrying  int64 `json:"abandoned_retrying"`
	AbandonedExhausted int64 `json:"abandoned_exhausted"`

	// AbandonedAttempts24h counts abandonments rather than abandoned chunks,
	// over the last day.
	//
	// The two counters above are stock levels — how many chunks are in that
	// state right now — and a stock cannot show a rate. A deployment that
	// loses an attempt every few minutes but retries successfully every time
	// reads as zero on both, because no chunk is left sitting in a bad state;
	// this is the number that moves.
	AbandonedAttempts24h int64 `json:"abandoned_attempts_24h"`
}

// ActiveDownloads maps a source id to the download chunk currently working on
// it.
//
// Only the in-flight part is asked of the queue. Whether a source was ever
// successfully installed is recorded on the source instead: terminal chunks
// are garbage collected, so a state read from them would be right on the day
// and wrong from the next one.
func (q *Queue) ActiveDownloads(ctx context.Context) (map[string]string, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, selection FROM chunk
		 WHERE kind = $1 AND status IN ($2,$3) AND selection <> ''`,
		KindDownload, StatusQueued, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, selection string
		if err := rows.Scan(&id, &selection); err != nil {
			return nil, err
		}
		for _, sid := range strings.Split(selection, ",") {
			if sid = strings.TrimSpace(sid); sid != "" {
				out[sid] = id
			}
		}
	}
	return out, rows.Err()
}

// Stats reports queue and throughput counters.
//
// One query rather than six: the counters are read together for a dashboard,
// and six round trips could disagree with each other while a chunk finished
// between them.
//
// The abandonment counters read chunk_attempt rather than inferring from
// chunk.attempts. Inferring was close but not correct: "status = error AND
// attempts >= MaxAttempts" counts a chunk that was abandoned twice and then
// genuinely failed on its last attempt as an abandonment, because a counter
// records that a chunk was claimed three times without recording what happened
// on any of them. Asking the attempt whose outcome is actually "abandoned"
// removes the guess.
func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	now := q.nowFn()
	var s Stats
	err := q.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status = $1),
		       count(*) FILTER (WHERE status = $2),
		       count(*) FILTER (WHERE status = $7),
		       count(*) FILTER (WHERE status = $3),
		       count(*) FILTER (WHERE status = $4),
		       coalesce(min(created_at) FILTER (WHERE status = $3), 0),
		       coalesce(sum(n_variants) FILTER (WHERE status = $1), 0),
		       count(*) FILTER (WHERE finished_at >= $5),
		       count(*) FILTER (WHERE finished_at >= $6),
		       count(*) FILTER (WHERE status = $3 AND lost),
		       count(*) FILTER (WHERE status = $2 AND last_lost)
		  FROM (
		    SELECT j.status, j.created_at, j.finished_at, j.n_variants,
		           -- Any attempt was abandoned: this chunk has been retried after
		           -- losing a worker.
		           EXISTS (SELECT 1 FROM chunk_attempt a
		                    WHERE a.chunk_id = j.id AND a.outcome = $8) AS lost,
		           -- The *last* attempt was abandoned: the chunk ended because it
		           -- ran out of attempts, not because it failed on its own.
		           (SELECT a.outcome = $8 FROM chunk_attempt a
		              WHERE a.chunk_id = j.id
		              ORDER BY a.attempt DESC LIMIT 1) AS last_lost
		      FROM chunk j
		  ) t`,
		StatusDone, StatusError, StatusQueued, StatusRunning,
		now-24*3600, now-7*24*3600, StatusCancelled, OutcomeAbandoned).
		Scan(&s.Total, &s.Succeeded, &s.Failed, &s.Cancelled, &s.Queued, &s.Running,
			&s.OldestQueuedAt, &s.Variants, &s.Last24h, &s.Last7d,
			&s.AbandonedRetrying, &s.AbandonedExhausted)
	if err != nil {
		return s, err
	}
	// Separate query because it counts a different thing: attempts, not
	// chunks, so it cannot share the FROM above without either double-counting
	// the chunk columns or duplicating rows across the join.
	err = q.pool.QueryRow(ctx,
		`SELECT count(*) FROM chunk_attempt WHERE outcome = $1 AND ended_at >= $2`,
		OutcomeAbandoned, now-24*3600).Scan(&s.AbandonedAttempts24h)
	return s, err
}
