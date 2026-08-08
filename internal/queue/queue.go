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
	// StatusCancelled is a job someone stopped on purpose.
	//
	// Its own status rather than an error with a message: a cancel is a
	// decision, not a fault, and counting one as a failure would make a
	// deliberate stop indistinguishable from something going wrong — on the
	// metrics page most of all, where the failure rate is what gets watched.
	StatusCancelled = "cancelled"
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
	// KindMove relocates a source's files between storage locations. A job
	// because it moves the same volume of data a download does, and because
	// only the worker can reach both ends.
	KindMove = "move"
)

// Postgres NOTIFY channels.
const (
	chanQueued = "job_queued"
	// chanCancel carries a job id whose run should stop. The worker holding it
	// may be in another process, which is the whole reason this goes through
	// the database rather than a method call.
	chanCancel = "job_cancel"
	chanDone   = "job_done"
)

// Job is one row of the job table (its metadata, without the input/result blobs).
type Job struct {
	ID        string `json:"job_id" doc:"Stable identifier. Poll and fetch results with it."`
	Kind      string `json:"kind" doc:"locus | vcf for annotation; download | move for provisioning."`
	Snapshot  string `json:"snapshot" doc:"The snapshot annotated against. An individual-source selection becomes a generated snapshot, which is what makes the run reproducible."`
	Selection string `json:"selection" doc:"The annotation fields asked for, or empty for the snapshot's defaults."`
	Status    string `json:"status" doc:"queued | running | done | error | cancelled."`
	Error     string `json:"error,omitempty" doc:"Why the job failed, when it did."`
	NVariants int64  `json:"n_variants" doc:"How many variants were annotated."`
	ClientIP  string `json:"client_ip,omitempty" doc:"The address the job was submitted from."`
	Session   string `json:"session,omitempty" doc:"The submitter's session, which scopes an anonymous caller's own history."`
	UserID    string `json:"user_id,omitempty" doc:"The owning account, when the submitter had one. Authoritative where session is not, being written from the credential the server verified."`
	Label     string `json:"label,omitempty" doc:"A short human label: the locus, or the submitted filename."`

	Weight     int   `json:"weight,omitempty" doc:"How many worker slots this job occupies while it runs. An annotation is 1; provisioning is heavier, because it saturates disk and CPU for hours and two at once finish later than one after the other."`
	CreatedAt  int64 `json:"created_at" doc:"Unix seconds."`
	StartedAt  int64 `json:"started_at,omitempty" doc:"Unix seconds. Absent until a worker claims it."`
	FinishedAt int64 `json:"finished_at,omitempty" doc:"Unix seconds. Absent until it finishes."`
}

// Terminal reports whether the job has reached a final status.
func (j Job) Terminal() bool {
	return j.Status == StatusDone || j.Status == StatusError || j.Status == StatusCancelled
}

// NewJob is the metadata for enqueuing a job (plus its input body).
type NewJob struct {
	Kind      string
	Snapshot  string
	Selection string
	ClientIP  string
	Session   string
	UserID    string
	Label     string
	// Weight is how much of the pool this job occupies; 0 means 1.
	Weight int
	Body   []byte
}

// weightOf normalizes a job's weight. 0 means the caller did not care, which is
// one slot — the same as an annotation.
func weightOf(w int) int {
	if w < 1 {
		return 1
	}
	return w
}

// jobCols is the SELECT list backing scanJob. NULLable columns are coalesced in
// SQL so the scan targets are plain Go values — the SQLite port used sql.NullXxx
// and then threw the validity flag away, which amounts to the same thing with
// more ceremony.
const jobCols = `id, kind, snapshot, selection, status, COALESCE(error,''), ` +
	`COALESCE(n_variants,0), client_ip, session_id, COALESCE(user_id,''), label, weight, created_at, ` +
	`COALESCE(started_at,0), COALESCE(finished_at,0)`

// jobColsJ is jobCols qualified with the "j" alias, for the claim query's join.
const jobColsJ = `j.id, j.kind, j.snapshot, j.selection, j.status, COALESCE(j.error,''), ` +
	`COALESCE(j.n_variants,0), j.client_ip, j.session_id, COALESCE(j.user_id,''), j.label, j.weight, j.created_at, ` +
	`COALESCE(j.started_at,0), COALESCE(j.finished_at,0)`

// ErrNotCancellable is returned when a job has already finished.
var ErrNotCancellable = errors.New("job is not running")

// ErrNoSuchJob is returned for an unknown id.
var ErrNoSuchJob = errors.New("no such job")

// Cancel stops a job.
//
// A queued job is settled here and never starts. A running one is signalled
// over NOTIFY, because the worker executing it is usually in another process —
// that is the whole reason this goes through the database rather than a method
// call. Its worker records the outcome, so a cancel does not race the run to
// write the row.
func (q *Queue) Cancel(ctx context.Context, id string) (Job, error) {
	// Settle it here if it has not started: no worker is involved, so there is
	// nothing to signal and nothing to wait for.
	row := q.pool.QueryRow(ctx, `
		UPDATE job SET status=$2, error='cancelled', finished_at=$3
		 WHERE id=$1 AND status=$4
		RETURNING `+jobCols, id, StatusCancelled, q.nowFn(), StatusQueued)
	job, err := scanJob(row)
	if err == nil {
		// Wake anyone in WaitFor: the job is terminal now.
		_, _ = q.pool.Exec(ctx, `SELECT pg_notify($1,$2)`, chanDone, id)
		return job, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Job{}, err
	}

	job, ok, err := q.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !ok {
		return Job{}, ErrNoSuchJob
	}
	if job.Status != StatusRunning {
		return job, ErrNotCancellable
	}
	if _, err := q.pool.Exec(ctx, `SELECT pg_notify($1,$2)`, chanCancel, id); err != nil {
		return Job{}, err
	}
	return job, nil
}

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
	dsn   string // retained so callers can open a second connection to the same database
	nowFn func() int64

	maxJobsPerIP int // per-IP concurrent running-job cap (<=0 = unlimited)
	// slots is the pool's total capacity in job weight, not job count. A job
	// runs only when the running set's weight plus its own fits. <=0 disables
	// the check, which is the pre-weight behaviour.
	slots int

	wg sync.WaitGroup

	mu      sync.Mutex
	waiters map[string][]chan struct{} // job id -> waiters blocked in WaitFor
	queued  chan struct{}              // wakes one worker (cap 1, non-blocking send)
	// running tracks jobs this process is executing, so a cancel arriving over
	// NOTIFY can reach the right subprocess. Only ever holds jobs claimed here;
	// a cancel for another replica's job finds nothing and is ignored, which is
	// correct — that replica is listening too.
	//
	// It is also exactly the set whose leases this process must renew: a job is
	// in here for as long as this process is really working on it.
	running map[string]*runningJob

	// workerID identifies this process in the claims it holds. Random per
	// process rather than derived from a hostname or pid, both of which a
	// container reuses across restarts — a restarted worker must not look like
	// the one that died, or it would appear to be renewing its predecessor's
	// leases.
	workerID string
	// leaseTTL is how long a claim stays valid without renewal, and leaseRenew
	// how often the holder refreshes it. The gap between them is the margin: a
	// worker has several chances to renew before anything reclaims its work, so
	// a slow query or a pause does not cost it a job it is actively running.
	leaseTTL   time.Duration
	leaseRenew time.Duration
}

// Default lease timings. The TTL is generous relative to the renew interval
// because the cost of the two errors is not symmetric: renewing more often than
// needed costs one tiny UPDATE, while expiring a lease a live worker still holds
// takes a job away mid-run and lets a second worker start it again.
const (
	// Sized for the longest thing a worker actually does, not for the shortest.
	//
	// Two minutes was too tight. A provisioning job runs for hours doing heavy
	// I/O, and a node under disk pressure can starve the process past that —
	// after which the holder is still working while a peer reclaims the job and
	// starts it again, both writing to one destination. Renewal is cheap and
	// frequent, so the margin here is a dozen missed renewals rather than six.
	defaultLeaseTTL   = 15 * time.Minute
	defaultLeaseRenew = 30 * time.Second
)

// MaxAttempts is how many times a job may be handed to a worker before it is
// failed instead of requeued.
//
// Only reached by jobs whose worker died without reporting anything: a job that
// fails cleanly is already terminal. Three is enough to ride out a rolling
// restart or a one-off eviction, and few enough that a job which kills its
// worker every time stops rather than doing so forever.
const MaxAttempts = 3

// Open connects to Postgres and prepares the queue.
//
// Jobs left running by a crashed worker are not touched here: see
// ReclaimExpired, and the comment on it for why recovering on open was wrong.
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
	id, err := newID()
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("worker id: %w", err)
	}
	q := &Queue{
		pool:       pool,
		dsn:        dsn,
		nowFn:      func() int64 { return time.Now().Unix() },
		waiters:    map[string][]chan struct{}{},
		queued:     make(chan struct{}, 1),
		running:    map[string]*runningJob{},
		workerID:   id,
		leaseTTL:   defaultLeaseTTL,
		leaseRenew: defaultLeaseRenew,
	}
	return q, nil
}

// SetLease overrides the claim lease timings. For tests, which cannot wait two
// minutes to watch one expire.
func (q *Queue) SetLease(ttl, renew time.Duration) {
	q.leaseTTL, q.leaseRenew = ttl, renew
}

// ReclaimExpired returns abandoned jobs to the queue, reporting how many.
//
// A job is abandoned when its lease has run out: the worker that claimed it
// renews while it works, so nobody renewing means nobody is working on it. That
// is a fact about the job rather than about the caller, which is what makes this
// safe to run from any process at any time — including while other workers are
// busy, and including several replicas running it at once.
//
// The predecessor of this was a blanket "requeue everything running" in Open.
// Both the API and the worker open the queue, so restarting the API reset jobs
// underneath the worker still executing them, and a multi-hour download could
// end up running twice against the same destination. The signal there was "a
// process started", which says nothing about whether anyone else is working.
//
// A NULL lease is expired: rows claimed before leases existed have nobody
// renewing them either, so they are abandoned by the same definition.
func (q *Queue) ReclaimExpired(ctx context.Context) (int, error) {
	// Past MaxAttempts the job is failed rather than requeued. Without this a
	// job that kills its worker every run came back forever, and each attempt
	// cost whatever the previous one had produced — the scratch of a killed
	// build is only cleaned up by the process that made it.
	//
	// The error text says what happened, because "attempt 3 of 3" is the part
	// an operator needs and the raw truth ("no worker ever reported on this")
	// is not otherwise visible anywhere.
	if _, err := q.pool.Exec(ctx, `
		UPDATE job
		   SET status=$1, claimed_by=NULL, lease_until=NULL, finished_at=$4,
		       error=$5
		 WHERE status=$2 AND COALESCE(lease_until, 0) < $3
		   AND attempts >= $6`,
		StatusError, StatusRunning, q.nowFn(), q.nowFn(),
		fmt.Sprintf("abandoned %d times without completing; not retried again", MaxAttempts),
		MaxAttempts); err != nil {
		return 0, fmt.Errorf("fail exhausted jobs: %w", err)
	}
	tag, err := q.pool.Exec(ctx, `
		UPDATE job
		   SET status=$1, started_at=NULL, claimed_by=NULL, lease_until=NULL
		 WHERE status=$2 AND COALESCE(lease_until, 0) < $3
		   AND attempts < $4`,
		StatusQueued, StatusRunning, q.nowFn(), MaxAttempts)
	if err != nil {
		return 0, fmt.Errorf("reclaim expired jobs: %w", err)
	}
	n := int(tag.RowsAffected())
	if n > 0 {
		// Wake a worker: these are claimable now, and nothing else will say so.
		// Enqueue is what normally notifies, and no enqueue happened here.
		_, _ = q.pool.Exec(ctx, `SELECT pg_notify($1,'')`, chanQueued)
	}
	return n, nil
}

// StartLeaseKeeper renews this process's claims and reclaims everyone's expired
// ones, until ctx ends.
//
// The two belong on the same timer because they are the same mechanism seen from
// either side: this process proves it is alive by renewing, and finds out others
// are not by looking for leases nobody renewed.
func (q *Queue) StartLeaseKeeper(ctx context.Context) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		t := time.NewTicker(q.leaseRenew)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := q.renewLeases(ctx); err != nil && ctx.Err() == nil {
					log.Printf("queue: renew leases: %v", err)
				}
				if n, err := q.ReclaimExpired(ctx); err != nil {
					if ctx.Err() == nil {
						log.Printf("queue: reclaim expired: %v", err)
					}
				} else if n > 0 {
					log.Printf("queue: reclaimed %d abandoned job(s)", n)
				}
			}
		}
	}()
}

// renewLeases extends the claims this process is actually executing.
//
// Driven by the in-process running set rather than by what the table says this
// worker holds: the point of a lease is to assert that work is really happening
// here, and only the running map knows that. A row still marked as ours that we
// are no longer running must be allowed to expire.
func (q *Queue) renewLeases(ctx context.Context) error {
	q.mu.Lock()
	ids := make([]string, 0, len(q.running))
	for id := range q.running {
		ids = append(ids, id)
	}
	q.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	_, err := q.pool.Exec(ctx, `
		UPDATE job SET lease_until=$1
		 WHERE id = ANY($2) AND status=$3 AND claimed_by=$4`,
		q.nowFn()+int64(q.leaseTTL.Seconds()), ids, StatusRunning, q.workerID)
	return err
}

// Close releases the connection pool.
func (q *Queue) Close() { q.pool.Close() }

// Ping checks the database is reachable. Used by the readiness probe.
func (q *Queue) Ping(ctx context.Context) error { return q.pool.Ping(ctx) }

// SetMaxJobsPerIP sets the per-IP concurrent running-job cap enforced by the fair
// scheduler (<=0 = unlimited). Call before starting workers.
func (q *Queue) SetMaxJobsPerIP(n int) { q.maxJobsPerIP = n }

// SetSlots sets the pool's capacity in job weight.
//
// Separate from the worker count on purpose: the goroutines decide how many
// jobs can be in flight at all, this decides how much work they may hold. A
// deployment that wants annotations to keep flowing during a provisioning run
// gives itself more slots than a download weighs.
func (q *Queue) SetSlots(n int) { q.slots = n }

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
		`INSERT INTO job (id,kind,snapshot,selection,status,client_ip,session_id,user_id,label,weight,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, j.Kind, j.Snapshot, j.Selection, StatusQueued,
		j.ClientIP, j.Session, j.UserID, j.Label, weightOf(j.Weight), q.nowFn()); err != nil {
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
// Input returns the body a job was submitted with.
//
// Retained rather than discarded once the job runs, which is what lets a VCF
// submission be answered with its own file annotated instead of a synthesised
// one carrying only the columns this server knows about.
func (q *Queue) Input(ctx context.Context, id string) ([]byte, bool, error) {
	var body []byte
	err := q.pool.QueryRow(ctx, `SELECT body FROM job_input WHERE job_id=$1`, id).Scan(&body)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return body, true, nil
}

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
		&j.Error, &j.NVariants, &j.ClientIP, &j.Session, &j.UserID, &j.Label, &j.Weight,
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
//
// An idle pool takes the next job whatever it weighs. Without that, a job heavier
// than the entire budget is unclaimable rather than merely exclusive: on a
// one-slot pool 0+2 <= 1 is false, so a weight-2 download waits behind nothing at
// all, forever and silently. VHW_WORKERS=1 is an ordinary deployment and slots
// follow workers, so that is a default configuration, not a corner. Weight is
// there to stop jobs overlapping, not to make them unrunnable — one too big for
// the pool should run alone, which is what an empty pool already guarantees.
const claimQuery = `
UPDATE job SET status = $1, started_at = $2, claimed_by = $6, lease_until = $7,
               attempts = job.attempts + 1
WHERE id = (
  SELECT j.id
  FROM job j
  LEFT JOIN (
    SELECT client_ip, COUNT(*) AS c FROM job WHERE status = $1 GROUP BY client_ip
  ) r ON r.client_ip = j.client_ip
  CROSS JOIN (
    SELECT COALESCE(SUM(weight),0) AS used FROM job WHERE status = $1
  ) p
  WHERE j.status = $3 AND COALESCE(r.c, 0) < $4
    AND (p.used = 0 OR p.used + j.weight <= $5)
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
	slots := q.slots
	if slots <= 0 {
		slots = 1 << 30
	}

	// The capacity check reads the running set, which SKIP LOCKED does not lock,
	// so two workers claiming at the same instant could both see room that only
	// one of them has. An advisory lock serializes the claim itself.
	//
	// It gives back some of what SKIP LOCKED bought, and that is the right
	// trade: a claim is a single indexed statement measured in microseconds,
	// while over-committing the budget is exactly the failure this exists to
	// prevent — two multi-hour downloads on one machine.
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return Job{}, nil, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "varhub-job-claim"); err != nil {
		return Job{}, nil, false, err
	}

	row := tx.QueryRow(ctx, claimQuery, StatusRunning, q.nowFn(), StatusQueued, maxPerIP, slots,
		q.workerID, q.nowFn()+int64(q.leaseTTL.Seconds()))
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, nil, false, nil
	}
	if err != nil {
		return Job{}, nil, false, err
	}
	// Read the body inside the same transaction, so a claim and its input are
	// one atomic step: committing the claim and then failing to read the body
	// would leave a job marked running that no worker is running.
	var body []byte
	if err := tx.QueryRow(ctx,
		`SELECT body FROM job_input WHERE job_id=$1`, job.ID).Scan(&body); err != nil {
		return Job{}, nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, nil, false, err
	}
	return job, body, true, nil
}

// process runs the job's runner and records its outcome.
// runningJob is a job executing in this process, and the handle to stop it.
type runningJob struct {
	cancel    context.CancelFunc
	cancelled bool // set when a cancel was requested, so the outcome says so
}

func (q *Queue) process(ctx context.Context, job Job, input []byte, runner Runner) {
	start := time.Now()
	log.Printf("queue: job %s running (kind=%s, ip=%s)", job.ID, job.Kind, job.ClientIP)

	// A context per job, so cancelling one does not touch the others this
	// worker has run or will run.
	runCtx, cancel := context.WithCancel(ctx)
	rj := &runningJob{cancel: cancel}
	q.mu.Lock()
	q.running[job.ID] = rj
	q.mu.Unlock()
	defer func() {
		cancel()
		q.mu.Lock()
		delete(q.running, job.ID)
		q.mu.Unlock()
	}()

	out, err := runner(runCtx, job, input)

	q.mu.Lock()
	cancelled := rj.cancelled
	q.mu.Unlock()
	if cancelled {
		// Whatever the subprocess reported on the way down, the reason it went
		// down is known and is not a failure.
		log.Printf("queue: job %s cancelled after %s",
			job.ID, time.Since(start).Round(time.Millisecond))
		q.finish(ctx, job.ID, StatusCancelled, "cancelled", Outcome{})
		return
	}
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

// RunningIDs returns the ids of jobs currently claimed and running.
//
// The queue is authoritative about what is in flight, which is what lets a
// worker reclaim another's leftovers without guessing: scratch named after a job
// not in this set belongs to nobody.
func (q *Queue) RunningIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := q.pool.Query(ctx, `SELECT id FROM job WHERE status=$1`, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		live[id] = true
	}
	return live, rows.Err()
}
