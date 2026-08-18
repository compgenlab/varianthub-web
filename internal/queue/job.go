package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Job statuses: the chunk's five, plus two for a submission made of parts.
//
// A chunk is never partial — it is one unit of work, and it is either waiting,
// running or finished. A job is what several of them add up to, and nine of
// twenty-six pieces annotated is neither "running" nor "queued" in any useful
// sense. Reporting it as one of those is how a caller ends up counting chunks
// themselves to find out what is happening.
const (
	// StatusPartialRunning: something has finished and something is running.
	StatusPartialRunning = "partial_running"
	// StatusPartialQueued: something has finished and nothing is running yet.
	// A split job spends a moment here between its last piece finishing and a
	// worker picking up the collect.
	StatusPartialQueued = "partial_queued"
)

// Job is a submission: what a caller sent, and how far it has got.
//
// Every submission is one, with at least one chunk. A locus list is a job of
// one chunk; a chromosome is a job of twenty-eight — a split, twenty-six
// pieces, and the collect that joins them. The id a caller is given and polls
// is a job's, and /jobs/{id} answers from here; a chunk never stands in for it.
//
// Every field below except the submitter's identity is read from the chunks,
// through the job_state view. Nothing writes a job's status: chunks are the
// unit of execution, so a status on this row would be a second answer to a
// question the chunks already answer, kept in step by every terminal path
// remembering to write it.
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

	// Chunks is how many chunks the job has, Done and Failed how many have
	// finished each way. Counted from the rows, so they always agree with what
	// a listing of those rows would show.
	//
	// A split's pieces appear as they are queued, so a VCF job counts 1 while
	// the split runs and 28 the moment it finishes. Status says what is
	// happening; these say how much of it there is.
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

	// PurgedAt is when this job's payload aged out, or 0 while it is still
	// there. The record is permanent; the input, the result, the rows behind
	// the table and the log are not — see queue.PurgeOlderThan.
	//
	// It is the difference between "this job had no results" and "this job's
	// results have expired", which nothing else on this row can express: after
	// a purge the counts read zero and the result chunk is empty, which is
	// exactly what a job that produced nothing looks like.
	PurgedAt int64 `json:"purged_at,omitempty"`

	// Runner is what executed the job — "local" for this deployment's own
	// worker pool. Empty for jobs that ran before it was recorded.
	Runner string `json:"runner,omitempty"`
}

// Purged reports whether the job's payload has aged out. A purged job still has
// its record: status, timing, variant count and who ran it.
func (j Job) Purged() bool { return j.PurgedAt > 0 }

// What kinds of thing execute a chunk.
//
// Recorded per chunk and frozen onto the job when its payload is swept, so the
// permanent record says what ran the work. RunnerSlurm is declared before there
// is anything to set it: the column has to exist from the first release that
// keeps records, or every job predating it becomes indistinguishable from a
// local one rather than merely unknown.
const (
	RunnerLocal = "local"
	RunnerSlurm = "slurm"
)

// Terminal reports whether the job has reached a final status.
func (j Job) Terminal() bool {
	return j.Status == StatusDone || j.Status == StatusError || j.Status == StatusCancelled
}

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
// A split's chunk produces more work rather than the answer, so it is not the
// one whose output gets served; every other kind is a job of one chunk that
// both starts and finishes it.
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

	// The submission is a VCF; splitting it is how the work starts, not what
	// was asked for. Keeping "split" off the job is what stops it reaching the
	// published kind field, where a caller who sent a file would have to learn
	// a third value to recognise their own submission.
	kind := n.Kind
	if kind == KindSplit {
		kind = KindVCF
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job (id,kind,snapshot,selection,label,client_ip,session_id,user_id,
		                 origin,input_chunk_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, kind, n.Snapshot, n.Selection, n.Label, n.ClientIP, n.Session,
		nullable(n.UserID), nullable(n.Origin), chunkID, q.nowFn()); err != nil {
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
		id, kind, n.ClientIP, n.Session, n.Selection, where)
	q.poke()
	return id, nil
}

// jobCols is the SELECT list backing scanJob. It reads job_state, not job:
// everything about how far a submission has got is derived from its chunks, and
// the view is where that is defined once.
const jobCols = `id, kind, snapshot, selection, COALESCE(label,''), status, ` +
	`COALESCE(error,''), n_variants, COALESCE(client_ip,''), ` +
	`COALESCE(session_id,''), COALESCE(user_id,''), COALESCE(origin,''), ` +
	`created_at, COALESCE(started_at,0), COALESCE(finished_at,0), ` +
	`chunks, done, failed, COALESCE(prefix,''), COALESCE(input_chunk_id,''), ` +
	`COALESCE(result_chunk_id,''), COALESCE(purged_at,0), COALESCE(runner,'')`

func scanJob(row rowScanner) (Job, error) {
	var j Job
	if err := row.Scan(&j.ID, &j.Kind, &j.Snapshot, &j.Selection, &j.Label,
		&j.Status, &j.Error, &j.NVariants, &j.ClientIP, &j.Session, &j.UserID,
		&j.Origin, &j.CreatedAt, &j.StartedAt, &j.FinishedAt,
		&j.Chunks, &j.Done, &j.Failed, &j.Prefix,
		&j.InputChunkID, &j.ResultChunkID, &j.PurgedAt, &j.Runner); err != nil {
		return Job{}, err
	}
	return j, nil
}

// GetJob reads a job by id.
func (q *Queue) GetJob(ctx context.Context, id string) (Job, bool, error) {
	j, err := scanJob(q.pool.QueryRow(ctx,
		`SELECT `+jobCols+` FROM job_state WHERE id=$1`, id))
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
	Status   string   // queued|running|partial_running|partial_queued|done|error|cancelled
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
	query := `SELECT ` + jobCols + ` FROM job_state`
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
// The one thing about a job that is written rather than derived, because it is
// not a fact about the chunks: it is an instruction that arrived from outside.
// A queued chunk is settled here and never starts, but a running one is only
// signalled — its worker records the outcome when the message reaches it, which
// may be seconds later and in another process. A caller who cancels and then
// polls must not be told the work is still running in the meantime, so the
// request itself is recorded and the derived status reads it first.
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

	// Recorded before the chunks are touched. The other order leaves a window
	// where the chunks are settled and the job still reads as running, which is
	// exactly what a poll immediately after a cancel would catch.
	if _, err := q.pool.Exec(ctx,
		`UPDATE job SET cancelled_at=$2 WHERE id=$1 AND cancelled_at IS NULL`,
		id, q.nowFn()); err != nil {
		return Job{}, err
	}

	rows, err := q.pool.Query(ctx,
		`SELECT id FROM chunk WHERE job_id=$1 AND finished_at IS NULL`, id)
	if err != nil {
		return Job{}, err
	}
	var live []string
	for rows.Next() {
		var chunkID string
		if err := rows.Scan(&chunkID); err != nil {
			rows.Close()
			return Job{}, err
		}
		live = append(live, chunkID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Job{}, err
	}

	for _, chunkID := range live {
		if _, err := q.cancelChunk(ctx, chunkID); err != nil &&
			!errors.Is(err, ErrNotCancellable) && !errors.Is(err, ErrNoSuchChunk) {
			return Job{}, err
		}
	}
	j, _, err = q.GetJob(ctx, id)
	return j, err
}

// JobChunks lists every chunk of a job: the split, the pieces it produced, the
// collect that joins them — in the order they were created, with the pieces in
// split order.
//
// All of them, not just the pieces. This is what GET /jobs/{id} returns
// alongside the status, and a listing that hid the split and the collect would
// leave a caller unable to find out why a job failed in either of them.
func (q *Queue) JobChunks(ctx context.Context, jobID string) ([]Chunk, error) {
	return q.chunksWhere(ctx,
		`WHERE job_id=$1 ORDER BY created_at, chunk_index NULLS FIRST, id`, jobID)
}

// SplitChunks lists a job's pieces in split order — without the split and
// collect that bracket them.
//
// Ordered by chunk_index, which is the order the file was cut in; joining them
// any other way produces a VCF whose records go backwards.
func (q *Queue) SplitChunks(ctx context.Context, jobID string) ([]Chunk, error) {
	return q.chunksWhere(ctx,
		`WHERE job_id = $1 AND chunk_index IS NOT NULL ORDER BY chunk_index`, jobID)
}

func (q *Queue) chunksWhere(ctx context.Context, clause string, args ...any) ([]Chunk, error) {
	rows, err := q.pool.Query(ctx, `SELECT `+chunkCols+` FROM chunk `+clause, args...)
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

// SetPrefix records where a split put the job's pieces.
//
// The only thing a split writes to the job. How many pieces there are, and how
// many have finished, are counted from the rows.
func (q *Queue) SetPrefix(ctx context.Context, jobID, prefix string) error {
	_, err := q.pool.Exec(ctx, `UPDATE job SET prefix=$2 WHERE id=$1`, jobID, prefix)
	return err
}
