// Package queue is the Postgres-backed async job queue: it persists jobs, their
// inputs, and their results, and drives a worker pool that annotates queued jobs.
//
// It is a port of the SQLite queue that lived in varianthub-cli's internal/server.
// Three things changed materially, and each is load-bearing:
//
//   - Claiming is one statement — UPDATE ... WHERE id = (SELECT ... FOR UPDATE OF j
//     SKIP LOCKED LIMIT 1) RETURNING. The SQLite version did SELECT-then-UPDATE and
//     relied on SetMaxOpenConns(1) to serialize workers; against a pool that races,
//     and losing the compare-and-set made a worker drop out of its drain loop.
//   - WaitFor blocks on a LISTEN/NOTIFY broadcast instead of polling every 150ms.
//     Polling a local file is cheap; polling over a socket, once per waiting HTTP
//     request, is not. NOTIFY also means a waiter in one replica learns about a job
//     finished by a worker in another.
//   - Schema lives in migrations/, not in an ad-hoc CREATE-IF-NOT-EXISTS plus an
//     ALTER TABLE loop that string-matched driver error text.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusError   = "error"
)

// Job kinds.
const (
	KindLocus = "locus"
	KindVCF   = "vcf"
	// KindDownload provisions a snapshot's source data instead of annotating. It
	// shares the queue so it gets the same persistence, scheduling and error
	// reporting; the worker dispatches on this.
	KindDownload = "download"
	// KindCleanup reclaims a removed source's files, for the same reason
	// downloads are jobs: only the worker mounts the storage.
	KindCleanup = "cleanup"
)

// Postgres NOTIFY channels.
const (
	chanQueued = "job_queued"
	chanDone   = "job_done"
)

// Job is one row of the job table (its metadata, without the input/result blobs).
type Job struct {
	ID        string `json:"job_id"`
	Kind      string `json:"kind"`
	Snapshot  string `json:"snapshot"`
	Selection string `json:"selection"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	NVariants int64  `json:"n_variants"`
	ClientIP  string `json:"client_ip,omitempty"`
	Session   string `json:"session,omitempty"` // submitter's session id, for scoping history
	// UserID is the owning account, when the submitter had one. Authoritative
	// where Session is not: a session id is asserted by the client, while this
	// is written from the credential the server verified.
	UserID     string `json:"user_id,omitempty"`
	Label      string `json:"label,omitempty"` // short human label (the locus, or the VCF filename)
	CreatedAt  int64  `json:"created_at"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

// Terminal reports whether the job has reached a final status.
func (j Job) Terminal() bool { return j.Status == StatusDone || j.Status == StatusError }

// NewJob is the metadata for enqueuing a job (plus its input body).
type NewJob struct {
	Kind      string
	Snapshot  string
	Selection string
	ClientIP  string
	Session   string
	UserID    string
	Label     string
	Body      []byte
}

// jobCols is the SELECT list backing scanJob. NULLable columns are coalesced in
// SQL so the scan targets are plain Go values — the SQLite port used sql.NullXxx
// and then threw the validity flag away, which amounts to the same thing with
// more ceremony.
const jobCols = `id, kind, snapshot, selection, status, COALESCE(error,''), ` +
	`COALESCE(n_variants,0), client_ip, session_id, COALESCE(user_id,''), label, created_at, ` +
	`COALESCE(started_at,0), COALESCE(finished_at,0)`

// jobColsJ is jobCols qualified with the "j" alias, for the claim query's join.
const jobColsJ = `j.id, j.kind, j.snapshot, j.selection, j.status, COALESCE(j.error,''), ` +
	`COALESCE(j.n_variants,0), j.client_ip, j.session_id, COALESCE(j.user_id,''), j.label, j.created_at, ` +
	`COALESCE(j.started_at,0), COALESCE(j.finished_at,0)`

// SetLog records what a job's run printed.
//
// Written separately from the outcome and best-effort at the call site: a log
// that fails to store must not turn a successful job into a failed one, and a
// failed job's log is worth keeping even when the failure itself is what is
// being recorded.
func (q *Queue) SetLog(ctx context.Context, id, output string) error {
	if output == "" {
		return nil
	}
	_, err := q.pool.Exec(ctx, `
		INSERT INTO job_log (job_id, output) VALUES ($1,$2)
		ON CONFLICT (job_id) DO UPDATE SET output = excluded.output`, id, output)
	return err
}

// Log returns what a job's run printed, and whether anything was recorded.
func (q *Queue) Log(ctx context.Context, id string) (string, bool, error) {
	var out string
	err := q.pool.QueryRow(ctx, `SELECT output FROM job_log WHERE job_id=$1`, id).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// Outcome is what a Runner produces for a completed job.
type Outcome struct {
	// Result is the annotation JSON, stored verbatim. It is what the CLI emitted,
	// and what the inline ?wait= path returns without touching job_variant.
	Result []byte
	// N is the number of variants.
	N int
	// Columns is the JSON column model for these results (may be nil). Stored on
	// the job so its results stay renderable even if the snapshot is re-pinned.
	Columns []byte
	// Variants says whether Result is an annotated-variant array that should be
	// projected into job_variant. A download's result is a file manifest.
	Variants bool
}

// Runner annotates one job's input. An error marks the job failed (its message
// is stored on the job).
type Runner func(ctx context.Context, job Job, input []byte) (Outcome, error)

// Queue is the job queue and its worker pool.
type Queue struct {
	pool  *pgxpool.Pool
	nowFn func() int64

	maxJobsPerIP int // per-IP concurrent running-job cap (<=0 = unlimited)

	wg sync.WaitGroup

	mu      sync.Mutex
	waiters map[string][]chan struct{} // job id -> waiters blocked in WaitFor
	queued  chan struct{}              // wakes one worker (cap 1, non-blocking send)
}

// Open connects to Postgres and prepares the queue. Jobs left "running" by a
// previous process (a crash) are reset to "queued" so they are re-processed.
//
// The caller must have applied migrations/ first; Open does not create schema.
func Open(ctx context.Context, dsn string) (*Queue, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	q := &Queue{
		pool:    pool,
		nowFn:   func() int64 { return time.Now().Unix() },
		waiters: map[string][]chan struct{}{},
		queued:  make(chan struct{}, 1),
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job SET status=$1, started_at=NULL WHERE status=$2`,
		StatusQueued, StatusRunning); err != nil {
		pool.Close()
		return nil, fmt.Errorf("requeue interrupted jobs: %w", err)
	}
	return q, nil
}

// Close releases the connection pool.
func (q *Queue) Close() { q.pool.Close() }

// Ping checks the database is reachable. Used by the readiness probe.
func (q *Queue) Ping(ctx context.Context) error { return q.pool.Ping(ctx) }

// SetMaxJobsPerIP sets the per-IP concurrent running-job cap enforced by the fair
// scheduler (<=0 = unlimited). Call before starting workers.
func (q *Queue) SetMaxJobsPerIP(n int) { q.maxJobsPerIP = n }

// newID returns a random 128-bit hex id.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Enqueue records a new queued job (metadata + input body) and wakes a worker.
func (q *Queue) Enqueue(ctx context.Context, j NewJob) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(ctx,
		`INSERT INTO job (id,kind,snapshot,selection,status,client_ip,session_id,user_id,label,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, j.Kind, j.Snapshot, j.Selection, StatusQueued,
		j.ClientIP, j.Session, j.UserID, j.Label, q.nowFn()); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO job_input (job_id,body) VALUES ($1,$2)`, id, j.Body); err != nil {
		return "", err
	}
	// NOTIFY fires on commit, so a listener never sees a job it cannot yet claim.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1,$2)`, chanQueued, id); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	log.Printf("queue: job %s queued (kind=%s, ip=%s, session=%s, selection=%q, %d bytes)",
		id, j.Kind, j.ClientIP, j.Session, j.Selection, len(j.Body))
	q.poke()
	return id, nil
}

// poke wakes one waiting worker in this process (non-blocking).
func (q *Queue) poke() {
	select {
	case q.queued <- struct{}{}:
	default:
	}
}

// Get returns a job's metadata (ok=false when the id is unknown).
func (q *Queue) Get(ctx context.Context, id string) (Job, bool, error) {
	row := q.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM job WHERE id=$1`, id)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// Result returns a done job's result JSON (ok=false when the id is unknown or the
// job has no stored result yet).
func (q *Queue) Result(ctx context.Context, id string) ([]byte, bool, error) {
	var js string
	err := q.pool.QueryRow(ctx, `SELECT json FROM job_result WHERE job_id=$1`, id).Scan(&js)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(js), true, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(row rowScanner) (Job, error) {
	var j Job
	if err := row.Scan(&j.ID, &j.Kind, &j.Snapshot, &j.Selection, &j.Status,
		&j.Error, &j.NVariants, &j.ClientIP, &j.Session, &j.UserID, &j.Label,
		&j.CreatedAt, &j.StartedAt, &j.FinishedAt); err != nil {
		return Job{}, err
	}
	return j, nil
}

// JobFilter narrows a List query. Empty fields are not constrained.
type JobFilter struct {
	Status   string   // queued|running|done|error
	Session  string   // scope to one submitter's session id
	UserID   string   // scope to one account
	ClientIP string   // scope to one client IP
	Kinds    []string // restrict to these job kinds
}

// List returns jobs newest-first matching the filter, with limit/offset paging.
func (q *Queue) List(ctx context.Context, f JobFilter, limit, offset int) ([]Job, error) {
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

// DeleteOlderThan removes terminal jobs (finished_at set) whose finished_at is
// before cutoff, along with their input and result blobs. Queued and running jobs
// are never touched. Returns the number of jobs deleted.
//
// The blobs go with the job via ON DELETE CASCADE, so unlike the SQLite version
// this is one statement rather than three inside a transaction.
func (q *Queue) DeleteOlderThan(ctx context.Context, cutoff int64) (int64, error) {
	tag, err := q.pool.Exec(ctx,
		`DELETE FROM job WHERE finished_at IS NOT NULL AND finished_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// StartSweeper launches a goroutine that deletes terminal jobs older than ttl,
// sweeping once immediately and then every interval until ctx is cancelled. A
// ttl <= 0 disables GC (the goroutine is not started).
func (q *Queue) StartSweeper(ctx context.Context, ttl, interval time.Duration) {
	if ttl <= 0 {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		sweep := func() {
			cutoff := q.nowFn() - int64(ttl.Seconds())
			if n, err := q.DeleteOlderThan(ctx, cutoff); err != nil {
				if ctx.Err() == nil {
					log.Printf("queue: job GC: %v", err)
				}
			} else if n > 0 {
				log.Printf("queue: job GC removed %d job(s) older than %s", n, ttl)
			}
		}
		sweep()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

// StartWorkers launches n worker goroutines that claim and process queued jobs
// until ctx is cancelled. Call Wait to block for their shutdown.
func (q *Queue) StartWorkers(ctx context.Context, n int, runner Runner) {
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		q.wg.Add(1)
		go q.worker(ctx, runner)
	}
}

// Wait blocks until all workers, the sweeper, and the listener have stopped.
func (q *Queue) Wait() { q.wg.Wait() }

func (q *Queue) worker(ctx context.Context, runner Runner) {
	defer q.wg.Done()
	// Fallback poll: covers a missed NOTIFY and the multi-replica case where a job
	// was enqueued by another process while this one held no listener.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		// Drain all currently-claimable jobs before sleeping.
		for {
			if ctx.Err() != nil {
				return
			}
			job, input, ok, err := q.claimNext(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("queue: claim job: %v", err)
				break
			}
			if !ok {
				break
			}
			q.process(ctx, job, input, runner)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.queued:
		case <-ticker.C:
		}
	}
}

// claimQuery claims the next job in a single statement.
//
// Fairness is unchanged from the SQLite version: among queued jobs prefer the
// client IP with the fewest already running (round-robin), skip any IP at the
// per-IP cap, and break ties by oldest created_at then id.
//
// FOR UPDATE OF j is required rather than a bare FOR UPDATE: Postgres refuses to
// lock the nullable side of an outer join, and r is a LEFT JOIN subquery. Locking
// only j is both legal and what we want. SKIP LOCKED is what lets N workers claim
// N distinct jobs concurrently instead of serializing on the same head-of-queue row.
const claimQuery = `
UPDATE job SET status = $1, started_at = $2
WHERE id = (
  SELECT j.id
  FROM job j
  LEFT JOIN (
    SELECT client_ip, COUNT(*) AS c FROM job WHERE status = $1 GROUP BY client_ip
  ) r ON r.client_ip = j.client_ip
  WHERE j.status = $3 AND COALESCE(r.c, 0) < $4
  ORDER BY COALESCE(r.c, 0) ASC, j.created_at ASC, j.id ASC
  FOR UPDATE OF j SKIP LOCKED
  LIMIT 1
)
RETURNING ` + jobCols

// claimNext atomically claims the next queued job, marking it running. ok=false
// when there is nothing claimable.
func (q *Queue) claimNext(ctx context.Context) (Job, []byte, bool, error) {
	// <=0 means unlimited, expressed as a large sentinel so the predicate never filters.
	maxPerIP := q.maxJobsPerIP
	if maxPerIP <= 0 {
		maxPerIP = 1 << 30
	}
	row := q.pool.QueryRow(ctx, claimQuery, StatusRunning, q.nowFn(), StatusQueued, maxPerIP)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, nil, false, nil
	}
	if err != nil {
		return Job{}, nil, false, err
	}
	var body []byte
	if err := q.pool.QueryRow(ctx,
		`SELECT body FROM job_input WHERE job_id=$1`, job.ID).Scan(&body); err != nil {
		return Job{}, nil, false, err
	}
	return job, body, true, nil
}

// process runs the job's runner and records its outcome.
func (q *Queue) process(ctx context.Context, job Job, input []byte, runner Runner) {
	start := time.Now()
	log.Printf("queue: job %s running (kind=%s, ip=%s)", job.ID, job.Kind, job.ClientIP)
	out, err := runner(ctx, job, input)
	if err != nil {
		log.Printf("queue: job %s failed after %s: %v",
			job.ID, time.Since(start).Round(time.Millisecond), err)
		q.finish(ctx, job.ID, StatusError, err.Error(), Outcome{})
		return
	}
	q.finish(ctx, job.ID, StatusDone, "", out)
	log.Printf("queue: job %s done (%d variant(s) in %s)",
		job.ID, out.N, time.Since(start).Round(time.Millisecond))
}

// finish records a job's terminal state and its result (if any), then notifies
// anyone blocked in WaitFor. Status, result and notification commit together, so
// a waiter woken by the NOTIFY always finds the row already terminal.
func (q *Queue) finish(ctx context.Context, id, status, errMsg string, out Outcome) {
	// Use a background context for the write: the job itself may have been
	// cancelled, but its outcome still has to be persisted.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	tx, err := q.pool.Begin(wctx)
	if err != nil {
		log.Printf("queue: finish job %s: %v", id, err)
		return
	}
	defer tx.Rollback(wctx) //nolint:errcheck // no-op after Commit

	if out.Result != nil {
		if _, err := tx.Exec(wctx,
			`INSERT INTO job_result (job_id,json) VALUES ($1,$2)
			 ON CONFLICT (job_id) DO UPDATE SET json = excluded.json`,
			id, string(out.Result)); err != nil {
			log.Printf("queue: store job %s result: %v", id, err)
			return
		}
		// Rows for querying. Same transaction as the blob and the status change, so
		// a job is never observably "done" with results that are not yet queryable.
		//
		// Skipped for a download: its result is a file manifest, not variants, and
		// forcing it through the variant projection would fail on a shape that was
		// never meant to fit.
		if out.Variants {
			if err := insertVariants(wctx, tx, id, out.Result); err != nil {
				log.Printf("queue: store job %s variants: %v", id, err)
				return
			}
		}
	}
	var errArg any
	if errMsg != "" {
		errArg = errMsg
	}
	var colArg any
	if len(out.Columns) > 0 {
		colArg = string(out.Columns)
	}
	if _, err := tx.Exec(wctx,
		`UPDATE job SET status=$1, error=$2, n_variants=$3, finished_at=$4, columns=$5 WHERE id=$6`,
		status, errArg, out.N, q.nowFn(), colArg, id); err != nil {
		log.Printf("queue: finish job %s: %v", id, err)
		return
	}
	if _, err := tx.Exec(wctx, `SELECT pg_notify($1,$2)`, chanDone, id); err != nil {
		log.Printf("queue: notify job %s: %v", id, err)
		return
	}
	if err := tx.Commit(wctx); err != nil {
		log.Printf("queue: commit job %s: %v", id, err)
		return
	}
	q.wake(id)
}
