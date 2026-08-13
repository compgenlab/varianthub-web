package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Job is a submission and the chunks it became.
type Job struct {
	ID string `json:"job_id"`
	// ChunkID is the chunk the submitter was given and still polls. It is the
	// split chunk: it exists before any other does, so there is something to
	// return from the request that created it, and something to show while the
	// split is running.
	ChunkID string `json:"chunk_id"`
	Chunks  int    `json:"chunks"`
	Done    int    `json:"done"`
	Failed  int    `json:"failed"`
	Prefix  string `json:"-"`
}

// Pending reports whether the split has not yet said how many chunks there
// are.
//
// The state a caller sees between submitting and the split finishing. Zero
// chunks is not "nothing to do", it is "not counted yet" — and a caller shown
// 0/0 complete would reasonably read it as finished.
func (j Job) Pending() bool { return j.Chunks == 0 }

// Complete reports whether every chunk has reported, successfully or not.
func (j Job) Complete() bool { return j.Chunks > 0 && j.Done+j.Failed >= j.Chunks }

// CreateJob records a job for a chunk that is about to be split.
func (q *Queue) CreateJob(ctx context.Context, chunkID, prefix string) (string, error) {
	id, err := NewID()
	if err != nil {
		return "", err
	}
	if _, err := q.pool.Exec(ctx, `
		INSERT INTO job (id, chunk_id, prefix, created_at) VALUES ($1,$2,$3,$4)`,
		id, chunkID, prefix, q.nowFn()); err != nil {
		return "", err
	}
	if _, err := q.pool.Exec(ctx,
		`UPDATE chunk SET job_id = $2 WHERE id = $1`, chunkID, id); err != nil {
		return "", err
	}
	return id, nil
}

// SetChunkCount records how many chunks the split produced.
//
// Written once, when the split knows. Until then the job reads as pending,
// which is what stops the completion check below from firing on a job that
// has not been counted: 0 done of 0 chunks is not finished, it is unstarted.
func (q *Queue) SetChunkCount(ctx context.Context, jobID string, n int) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE job SET chunks = $2 WHERE id = $1`, jobID, n)
	return err
}

// GetJob reads a job by id.
func (q *Queue) GetJob(ctx context.Context, id string) (Job, bool, error) {
	var j Job
	err := q.pool.QueryRow(ctx, `
		SELECT id, chunk_id, chunks, done, failed, prefix FROM job WHERE id = $1`, id).
		Scan(&j.ID, &j.ChunkID, &j.Chunks, &j.Done, &j.Failed, &j.Prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	return j, err == nil, err
}

// JobForChunk reads the job a chunk belongs to, if any.
func (q *Queue) JobForChunk(ctx context.Context, chunkID string) (Job, bool, error) {
	var j Job
	err := q.pool.QueryRow(ctx, `
		SELECT b.id, b.chunk_id, b.chunks, b.done, b.failed, b.prefix
		  FROM job b JOIN chunk c ON c.job_id = b.id
		 WHERE c.id = $1`, chunkID).
		Scan(&j.ID, &j.ChunkID, &j.Chunks, &j.Done, &j.Failed, &j.Prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	return j, err == nil, err
}

// ChunkFinished records one chunk's outcome and reports whether that was the
// last one.
//
// The counter is bumped and read in a single statement, which is the whole
// point of it being a counter. Asked as a query over sibling chunks — "are
// they all terminal yet" — two chunks finishing at the same instant would both
// see the other's row already updated and both answer yes, and the collect
// step would run twice. Here exactly one caller sees the count reach the
// total.
//
// last is false for a job still pending its chunk count, because a split that
// has not finished cannot have produced the last chunk.
func (q *Queue) ChunkFinished(ctx context.Context, jobID string, ok bool) (last bool, err error) {
	col := "failed"
	if ok {
		col = "done"
	}
	var j Job
	err = q.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE job SET %s = %s + 1 WHERE id = $1
		RETURNING chunks, done, failed`, col, col), jobID).
		Scan(&j.Chunks, &j.Done, &j.Failed)
	if err != nil {
		return false, err
	}
	return j.Complete(), nil
}

// JobChunks lists a job's chunks in split order.
//
// Ordered by chunk_index, which is the order the file was cut in — joining
// them any other way produces a VCF whose records go backwards.
func (q *Queue) JobChunks(ctx context.Context, jobID string) ([]Chunk, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT `+chunkCols+` FROM chunk
		  WHERE job_id = $1 AND chunk_index IS NOT NULL
		  ORDER BY chunk_index`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chunk
	for rows.Next() {
		c, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetResultVCF records where a chunk's answer-as-a-VCF ended up, for a chunk
// that did not build it itself.
//
// The collect step's output belongs to the chunk the submitter was given, not
// to the collect chunk they never saw. Without this a job finishes with its
// answer filed under an id nobody has: the split chunk reports done, its
// export finds nothing, and the file sits in storage unreachable.
func (q *Queue) SetResultVCF(ctx context.Context, chunkID, uri string) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO chunk_result (chunk_id, json, vcf_uri) VALUES ($1, NULL, $2)
		ON CONFLICT (chunk_id) DO UPDATE SET vcf_uri = excluded.vcf_uri`, chunkID, uri)
	return err
}
