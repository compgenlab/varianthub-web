package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Job is a submission: what a caller sent, and how far it has got.
//
// Every submission is one, with at least one chunk. A locus list is a job of
// one chunk; a chromosome is a job of twenty-six. The id a caller is given and
// polls is a job's, and /jobs/{id} answers from this row — a chunk is reachable
// underneath it, at /jobs/{id}/chunks/{chunkID}, and never stands in for it.
//
// Status here is written rather than derived from the chunks. See
// migration 0037: aggregating would read a split job as done in the window
// between its last piece finishing and the collect that joins them being
// queued, which is exactly when a caller is polling hardest.
type Job struct {
	ID        string `json:"job_id"`
	Kind      string `json:"kind"`
	Snapshot  string `json:"snapshot"`
	Selection string `json:"selection"`
	Label     string `json:"label,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	NVariants int64  `json:"n_variants"`

	// Who submitted it. Not serialized by the API — see api.JobStatusResponse,
	// which projects this row rather than returning it.
	ClientIP string `json:"-"`
	Session  string `json:"-"`
	UserID   string `json:"-"`
	Origin   string `json:"origin,omitempty"`

	CreatedAt  int64 `json:"created_at"`
	StartedAt  int64 `json:"started_at,omitempty"`
	FinishedAt int64 `json:"finished_at,omitempty"`

	// Chunks is how many chunks the submission became, and Done and Failed how
	// many have reported. One and one for anything that was not split, counted
	// at submit, so a client reads the same three fields on every job rather
	// than having to ask whether this one was cut up.
	//
	// Zero means a split has not counted its pieces yet; see Pending.
	Chunks int `json:"chunks"`
	Done   int `json:"done"`
	Failed int `json:"failed"`

	// Prefix is where a split's pieces live. Empty for a job that was never
	// split, which owns no pieces to put anywhere.
	Prefix string `json:"-"`

	// InputChunkID holds the submitted input; ResultChunkID holds the answer,
	// and is empty until the job is done. For a one-chunk job they are the same
	// chunk.
	InputChunkID  string `json:"-"`
	ResultChunkID string `json:"-"`
}

// Terminal reports whether the job has reached a final status.
func (j Job) Terminal() bool {
	return j.Status == StatusDone || j.Status == StatusError || j.Status == StatusCancelled
}

// Pending reports whether a split has not yet said how many chunks there are.
//
// Only a split is ever pending: everything else is counted at submit. This is
// the state a caller sees between submitting a VCF and the split finishing.
// Zero chunks is not "nothing to do", it is "not counted yet" — and a caller
// shown 0/0 complete would reasonably read it as finished.
func (j Job) Pending() bool { return j.Chunks == 0 }

// Complete reports whether every counted chunk has reported, successfully or
// not.
func (j Job) Complete() bool { return j.Chunks > 0 && j.Done+j.Failed >= j.Chunks }

// NewJob is a submission: its terms, and its input.
//
// The same fields the first chunk is created with, because they are the same
// facts — the chunk gets its own copy so the claim query stays one statement
// over one table. See Submit.
type NewJob struct {
	// ID is the job's identifier, minted by NewID. Empty means Submit mints
	// one, which is what every caller that has no reason to care does.
	//
	// Supplied only by a caller that had to name the job before creating it —
	// an upload writes its object to jobs/<id>/ so a bucket listing says which
	// job each object belongs to. Never taken from a request: see NewID.
	ID            string
	Kind          string
	Snapshot      string
	Selection     string
	ClientIP      string
	Session       string
	UserID        string
	Label         string
	Weight        int
	MaxConcurrent int
	Origin        string
	MaxVariants   int

	// Body is the input itself, for submissions small enough to be worth
	// carrying; InputURI locates it in job storage otherwise. Exactly one.
	Body     []byte
	InputURI string
}

// Submit records a submission — one job and the chunk that starts it — and
// wakes a worker.
//
// The chunk completes the job unless the kind is a split, which finishes by
// producing more work rather than by producing the answer.
func (q *Queue) Submit(ctx context.Context, n NewJob) (string, error) {
	id := n.ID
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return "", err
		}
	} else if !idPattern.MatchString(id) {
		return "", fmt.Errorf("job id %q is not one NewID produced", id)
	}
	chunkID, err := NewID()
	if err != nil {
		return "", err
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// One chunk, counted from the start — unless this is a split, which does
	// not know yet how many it will produce and reads as pending until it does.
	//
	// Counting the sole chunk here is what lets a client read chunks/done/failed
	// on every job rather than having to ask whether it was split first. A job
	// of one and a job of twenty-six are then the same shape.
	chunks := 1
	kind := n.Kind
	if n.Kind == KindSplit {
		chunks = 0
		// The submission is a VCF; splitting it is how the work starts, not
		// what was asked for. Keeping "split" off the job is what stops it
		// reaching the published kind field, where a caller who sent a file
		// would have to learn a third value to recognise their own submission.
		kind = KindVCF
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job (id,kind,snapshot,selection,label,client_ip,session_id,user_id,
		                 origin,status,chunks,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		id, kind, n.Snapshot, n.Selection, n.Label, n.ClientIP, n.Session,
		nullable(n.UserID), nullable(n.Origin), StatusQueued, chunks,
		q.nowFn()); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE job SET input_chunk_id = $2 WHERE id = $1`, id, chunkID); err != nil {
		return "", err
	}

	if err := insertChunk(ctx, tx, q.nowFn(), NewChunk{
		ID:            chunkID,
		JobID:         id,
		Kind:          n.Kind,
		Snapshot:      n.Snapshot,
		Selection:     n.Selection,
		ClientIP:      n.ClientIP,
		Session:       n.Session,
		UserID:        n.UserID,
		Label:         n.Label,
		Weight:        n.Weight,
		MaxConcurrent: n.MaxConcurrent,
		Origin:        n.Origin,
		MaxVariants:   n.MaxVariants,
		CompletesJob:  n.Kind != KindSplit,
		Body:          n.Body,
		InputURI:      n.InputURI,
	}); err != nil {
		return "", err
	}
	// NOTIFY fires on commit, so a listener never sees a chunk it cannot yet
	// claim.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1,$2)`, chanQueued, chunkID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	where := fmt.Sprintf("%d bytes", len(n.Body))
	if n.InputURI != "" {
		where = "input at " + n.InputURI
	}
	log.Printf("queue: job %s submitted (kind=%s, ip=%s, session=%s, selection=%q, %s)",
		id, n.Kind, n.ClientIP, n.Session, n.Selection, where)
	q.poke()
	return id, nil
}

// jobCols is the SELECT list backing scanJob.
const jobCols = `id, COALESCE(kind,''), COALESCE(snapshot,''), COALESCE(selection,''), ` +
	`COALESCE(label,''), status, COALESCE(error,''), COALESCE(n_variants,0), ` +
	`COALESCE(client_ip,''), COALESCE(session_id,''), COALESCE(user_id,''), ` +
	`COALESCE(origin,''), created_at, COALESCE(started_at,0), COALESCE(finished_at,0), ` +
	`chunks, done, failed, COALESCE(prefix,''), COALESCE(input_chunk_id,''), ` +
	`COALESCE(result_chunk_id,'')`

func scanJob(row rowScanner) (Job, error) {
	var j Job
	if err := row.Scan(&j.ID, &j.Kind, &j.Snapshot, &j.Selection, &j.Label,
		&j.Status, &j.Error, &j.NVariants, &j.ClientIP, &j.Session, &j.UserID,
		&j.Origin, &j.CreatedAt, &j.StartedAt, &j.FinishedAt,
		&j.Chunks, &j.Done, &j.Failed, &j.Prefix,
		&j.InputChunkID, &j.ResultChunkID); err != nil {
		return Job{}, err
	}
	return j, nil
}

// GetJob reads a job by id.
func (q *Queue) GetJob(ctx context.Context, id string) (Job, bool, error) {
	j, err := scanJob(q.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM job WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// JobFilter narrows a ListJobs query. Empty fields are not constrained.
type JobFilter struct {
	Status   string   // queued|running|done|error|cancelled
	Session  string   // scope to one submitter's session id
	UserID   string   // scope to one account
	ClientIP string   // scope to one client IP
	Kinds    []string // restrict to these job kinds
}

// ListJobs returns jobs newest-first matching the filter, with limit/offset
// paging.
func (q *Queue) ListJobs(ctx context.Context, f JobFilter, limit, offset int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var where []string
	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Status != "" {
		add("status=$%d", f.Status)
	}
	if f.Session != "" {
		add("session_id=$%d", f.Session)
	}
	if f.UserID != "" {
		add("user_id=$%d", f.UserID)
	}
	if f.ClientIP != "" {
		add("client_ip=$%d", f.ClientIP)
	}
	if len(f.Kinds) > 0 {
		args = append(args, f.Kinds)
		where = append(where, fmt.Sprintf("kind = ANY($%d)", len(args)))
	}
	query := `SELECT ` + jobCols + ` FROM job`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	args = append(args, limit, offset)
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		len(args)-1, len(args))

	rows, err := q.pool.Query(ctx, query, args...)
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

// ErrNoSuchJob is returned for an unknown job id.
var ErrNoSuchJob = errors.New("no such job")

// CancelJob stops a job by stopping every chunk of it that has not finished.
//
// A job is cancelled the moment it is asked for, not when its chunks get round
// to noticing: a caller who cancels and then polls must not be told the work is
// still running. Each chunk is settled or signalled the way a single one always
// was — queued ones here, running ones over NOTIFY to whichever worker holds
// them, which is usually another process.
func (q *Queue) CancelJob(ctx context.Context, id string) (Job, error) {
	j, ok, err := q.GetJob(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !ok {
		return Job{}, ErrNoSuchJob
	}
	if j.Terminal() {
		return j, ErrNotCancellable
	}

	rows, err := q.pool.Query(ctx,
		`SELECT id, status FROM chunk WHERE job_id=$1 AND finished_at IS NULL`, id)
	if err != nil {
		return Job{}, err
	}
	type live struct{ id, status string }
	var chunks []live
	for rows.Next() {
		var c live
		if err := rows.Scan(&c.id, &c.status); err != nil {
			rows.Close()
			return Job{}, err
		}
		chunks = append(chunks, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Job{}, err
	}

	for _, c := range chunks {
		if _, err := q.cancelChunk(ctx, c.id); err != nil &&
			!errors.Is(err, ErrNotCancellable) && !errors.Is(err, ErrNoSuchChunk) {
			return Job{}, err
		}
	}

	// The job's own state, written whatever the chunks did. A running chunk is
	// only signalled, and its worker records its outcome whenever it gets the
	// message; the job must not sit at "running" until then.
	if _, err := q.pool.Exec(ctx, `
		UPDATE job SET status=$2, error=$3, finished_at=$4
		 WHERE id=$1 AND status NOT IN ($5,$6,$7)`,
		id, StatusCancelled, "cancelled", q.nowFn(),
		StatusDone, StatusError, StatusCancelled); err != nil {
		return Job{}, err
	}
	j, _, err = q.GetJob(ctx, id)
	return j, err
}

// JobChunks lists every chunk of a job: the split, the pieces it produced, the
// collect that joins them — in the order they were created, with the pieces in
// split order.
//
// All of them, not just the pieces. This is what /jobs/{id}/chunks answers, and
// a listing that hid the split and the collect would leave a caller unable to
// find out why a job failed in either of them.
func (q *Queue) JobChunks(ctx context.Context, jobID string) ([]Chunk, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT `+chunkCols+` FROM chunk WHERE job_id=$1
		  ORDER BY created_at, chunk_index NULLS FIRST, id`, jobID)
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

// JobChunk reads one chunk of one job.
//
// Scoped to the job rather than looked up by chunk id alone, because that is
// the shape of the route: a chunk is reachable underneath the job that owns it,
// so a caller entitled to the job is entitled to its chunks and no others.
func (q *Queue) JobChunk(ctx context.Context, jobID, chunkID string) (Chunk, bool, error) {
	c, err := scanChunk(q.pool.QueryRow(ctx,
		`SELECT `+chunkCols+` FROM chunk WHERE job_id=$1 AND id=$2`, jobID, chunkID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Chunk{}, false, nil
	}
	if err != nil {
		return Chunk{}, false, err
	}
	return c, true, nil
}

// SplitChunks lists a job's pieces in split order — the counted chunks, without
// the split and collect that bracket them.
//
// Ordered by chunk_index, which is the order the file was cut in; joining them
// any other way produces a VCF whose records go backwards.
func (q *Queue) SplitChunks(ctx context.Context, jobID string) ([]Chunk, error) {
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

// SetChunkCount records how many pieces the split produced, and where they
// live.
//
// Written once, when the split knows. Until then the job reads as pending,
// which is what stops the completion check from firing on a job that has not
// been counted: 0 done of 0 chunks is not finished, it is unstarted.
func (q *Queue) SetChunkCount(ctx context.Context, jobID, prefix string, n int) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE job SET chunks = $2, prefix = $3 WHERE id = $1`, jobID, n, prefix)
	return err
}

// ChunkFinished records one piece's outcome and reports whether that was the
// last one.
//
// The counter is bumped and read in a single statement, which is the whole
// point of it being a counter. Asked as a query over sibling chunks — "is every
// piece terminal yet" — two finishing at the same instant would both see the
// other's row already updated and both answer yes, and the collect step would
// run twice. Here exactly one caller sees the count reach the total.
//
// last is false for a job still pending its chunk count, because a split that
// has not finished cannot have produced the last piece.
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
