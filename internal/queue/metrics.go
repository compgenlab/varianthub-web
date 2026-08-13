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
	// Cancelled is counted apart from failed: someone stopping a job on
	// purpose is not the deployment going wrong, and folding the two together
	// would make the failure rate move for reasons nobody needs to chase.
	Cancelled int64 `json:"cancelled"`

	// What is in flight right now.
	Queued  int64 `json:"queued"`
	Running int64 `json:"running"`

	// OldestQueuedAt is the creation time of the longest-waiting queued job, 0
	// when nothing is waiting. A depth alone does not distinguish a queue that
	// is moving from one that is stuck.
	OldestQueuedAt int64 `json:"oldest_queued_at,omitempty"`

	// Variants counts variants across jobs that finished successfully. Failed
	// jobs are excluded: their n_variants is what was submitted, not what was
	// annotated, and counting it would inflate the total exactly when something
	// is going wrong.
	Variants int64 `json:"variants"`

	// Recent activity, by completion time.
	Last24h int64 `json:"last_24h"`
	Last7d  int64 `json:"last_7d"`

	// Abandoned counts jobs whose worker was killed rather than reporting —
	// currently retrying, plus those that gave up. Counted apart from Failed
	// because it is a different kind of trouble: a failure is the job going
	// wrong, an abandonment is the process running it disappearing, and the
	// usual cause is the container's memory limit.
	//
	// Retrying and exhausted are separate because they answer different
	// questions. A steady trickle of retries that eventually succeed is a
	// deployment losing capacity; exhausted jobs are work that was thrown away.
	AbandonedRetrying  int64 `json:"abandoned_retrying"`
	AbandonedExhausted int64 `json:"abandoned_exhausted"`
}

// ActiveDownloads maps a source id to the download job currently working on it.
//
// Only the in-flight part is asked of the queue. Whether a source was ever
// successfully installed is recorded on the source instead: terminal jobs are
// garbage collected, so a state read from them would be right on the day and
// wrong from the next one.
func (q *Queue) ActiveDownloads(ctx context.Context) (map[string]string, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, selection FROM job
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
// One query rather than six: the counters are read together for a dashboard, and
// six round trips could disagree with each other while a job finished between
// them.
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
		       count(*) FILTER (WHERE status = $3 AND attempts > 0),
		       count(*) FILTER (WHERE status = $2 AND attempts >= $8)
		  FROM job`,
		StatusDone, StatusError, StatusQueued, StatusRunning,
		now-24*3600, now-7*24*3600, StatusCancelled, MaxAttempts).
		Scan(&s.Total, &s.Succeeded, &s.Failed, &s.Cancelled, &s.Queued, &s.Running,
			&s.OldestQueuedAt, &s.Variants, &s.Last24h, &s.Last7d,
			&s.AbandonedRetrying, &s.AbandonedExhausted)
	return s, err
}
