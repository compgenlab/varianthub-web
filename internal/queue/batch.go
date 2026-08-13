package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Batch is a submission that became several jobs.
type Batch struct {
	ID     string `json:"batch_id"`
	JobID  string `json:"job_id"`
	Chunks int    `json:"chunks"`
	Done   int    `json:"done"`
	Failed int    `json:"failed"`
	Prefix string `json:"-"`
}

// Pending reports whether the split has not yet said how many chunks there are.
//
// The state a caller sees between submitting and the split finishing. Zero
// chunks is not "nothing to do", it is "not counted yet" — and a caller shown
// 0/0 complete would reasonably read it as finished.
func (b Batch) Pending() bool { return b.Chunks == 0 }

// Complete reports whether every chunk has reported, successfully or not.
func (b Batch) Complete() bool { return b.Chunks > 0 && b.Done+b.Failed >= b.Chunks }

// CreateBatch records a batch for a job that is about to be split.
func (q *Queue) CreateBatch(ctx context.Context, jobID, prefix string) (string, error) {
	id, err := NewID()
	if err != nil {
		return "", err
	}
	if _, err := q.pool.Exec(ctx, `
		INSERT INTO batch (id, job_id, prefix, created_at) VALUES ($1,$2,$3,$4)`,
		id, jobID, prefix, q.nowFn()); err != nil {
		return "", err
	}
	if _, err := q.pool.Exec(ctx,
		`UPDATE job SET batch_id = $2 WHERE id = $1`, jobID, id); err != nil {
		return "", err
	}
	return id, nil
}

// SetChunkCount records how many chunks the split produced.
//
// Written once, when the split knows. Until then the batch reads as pending,
// which is what stops the completion check below from firing on a batch that
// has not been counted: 0 done of 0 chunks is not finished, it is unstarted.
func (q *Queue) SetChunkCount(ctx context.Context, batchID string, n int) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE batch SET chunks = $2 WHERE id = $1`, batchID, n)
	return err
}

// GetBatch reads a batch by id.
func (q *Queue) GetBatch(ctx context.Context, id string) (Batch, bool, error) {
	var b Batch
	err := q.pool.QueryRow(ctx, `
		SELECT id, job_id, chunks, done, failed, prefix FROM batch WHERE id = $1`, id).
		Scan(&b.ID, &b.JobID, &b.Chunks, &b.Done, &b.Failed, &b.Prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, false, nil
	}
	return b, err == nil, err
}

// BatchForJob reads the batch a job belongs to, if any.
func (q *Queue) BatchForJob(ctx context.Context, jobID string) (Batch, bool, error) {
	var b Batch
	err := q.pool.QueryRow(ctx, `
		SELECT b.id, b.job_id, b.chunks, b.done, b.failed, b.prefix
		  FROM batch b JOIN job j ON j.batch_id = b.id
		 WHERE j.id = $1`, jobID).
		Scan(&b.ID, &b.JobID, &b.Chunks, &b.Done, &b.Failed, &b.Prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, false, nil
	}
	return b, err == nil, err
}

// ChunkFinished records one chunk's outcome and reports whether that was the
// last one.
//
// The counter is bumped and read in a single statement, which is the whole
// point of it being a counter. Asked as a query over sibling jobs — "are they
// all terminal yet" — two chunks finishing at the same instant would both see
// the other's row already updated and both answer yes, and the collect step
// would run twice. Here exactly one caller sees the count reach the total.
//
// last is false for a batch still pending its chunk count, because a split that
// has not finished cannot have produced the last chunk.
func (q *Queue) ChunkFinished(ctx context.Context, batchID string, ok bool) (last bool, err error) {
	col := "failed"
	if ok {
		col = "done"
	}
	var b Batch
	err = q.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE batch SET %s = %s + 1 WHERE id = $1
		RETURNING chunks, done, failed`, col, col), batchID).
		Scan(&b.Chunks, &b.Done, &b.Failed)
	if err != nil {
		return false, err
	}
	return b.Complete(), nil
}

// BatchChunks lists a batch's chunk jobs in split order.
//
// Ordered by chunk_index, which is the order the file was cut in — joining them
// any other way produces a VCF whose records go backwards.
func (q *Queue) BatchChunks(ctx context.Context, batchID string) ([]Job, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT `+jobCols+` FROM job
		  WHERE batch_id = $1 AND chunk_index IS NOT NULL
		  ORDER BY chunk_index`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
