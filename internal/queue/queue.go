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
	"regexp"
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

// How one attempt at a job ended, in job_attempt.outcome.
//
// Three of the four are the job statuses, deliberately: an attempt that
// finished is an attempt that put the job into that state, and giving them
// separate spellings would mean two vocabularies for one fact.
//
// The fourth has no status because it is not something a job can be. A job
// abandoned twice and then completed is a done job; only the attempts remember
// that anything went wrong, which is the whole reason they are recorded.
const (
	OutcomeDone      = StatusDone
	OutcomeError     = StatusError
	OutcomeCancelled = StatusCancelled
	// OutcomeAbandoned is an attempt whose worker stopped renewing the lease —
	// killed rather than failing, so it never reported anything.
	OutcomeAbandoned = "abandoned"
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

	Weight     int    `json:"weight,omitempty" doc:"How many worker slots this job occupies while it runs. An annotation is 1; provisioning is heavier, because it saturates disk and CPU for hours and two at once finish later than one after the other."`
	Origin     string `json:"origin,omitempty" doc:"How the job was submitted: \"web\" from a browser session, \"api\" from a personal access token. Absent for jobs recorded before this was tracked."`
	CreatedAt  int64  `json:"created_at" doc:"Unix seconds."`
	StartedAt  int64  `json:"started_at,omitempty" doc:"Unix seconds. Absent until a worker claims it."`
	FinishedAt int64  `json:"finished_at,omitempty" doc:"Unix seconds. Absent until it finishes."`

	// MaxVariants is the variant cap this job was admitted under; 0 is
	// unlimited. Stamped at submit, so the terms a job runs under are the ones
	// that applied when it was accepted.
	//
	// Not serialized: it is an internal admission decision, and a caller who
	// wants to know their limit asks about their account rather than reading it
	// off somebody's job.
	MaxVariants int `json:"-"`

	// InputURI is where this job's input is stored, set only on the Job a claim
	// returns — Get and List do not fill it, because nothing reading a job's
	// status needs it.
	//
	// Never serialized. It names a bucket and key inside the deployment, which
	// is an operator's business and not something a job status response should
	// hand to whoever asks. The rest of this struct is the published API; this
	// is not part of it.
	InputURI string `json:"-"`
}

// Terminal reports whether the job has reached a final status.
func (j Job) Terminal() bool {
	return j.Status == StatusDone || j.Status == StatusError || j.Status == StatusCancelled
}

// NewJob is the metadata for enqueuing a job (plus its input).
type NewJob struct {
	// ID is the job's identifier, minted by NewID. Empty means Enqueue mints
	// one, which is what every caller that has no reason to care does.
	//
	// Supplied only by a caller that had to name the job before creating it —
	// an upload writes its object to jobs/<id>/ so a bucket listing says which
	// job each object belongs to. Never taken from a request: see NewID.
	ID        string
	Kind      string
	Snapshot  string
	Selection string
	ClientIP  string
	Session   string
	UserID    string
	Label     string
	// Weight is how much of the pool this job occupies; 0 means 1.
	Weight int
	// MaxConcurrent caps how many of this submitter's jobs may run at once. 0
	// falls back to the deployment's per-IP cap, which is what anonymous work
	// and anything submitted before tiers existed gets.
	//
	// Recorded on the row rather than read from the account at dispatch: the
	// claim query stays one statement over one table, a job keeps the terms it
	// was admitted under, and an anonymous job has no account to read from.
	MaxConcurrent int
	// Origin is how the job arrived: OriginWeb, OriginAPI, or empty when
	// unrecorded. Reporting only — it decides nothing.
	Origin string
	// MaxVariants caps how many variants this job may carry; 0 is unlimited.
	// Recorded on the row so the worker enforcing it does not have to resolve
	// an account that may not exist.
	MaxVariants int

	// Body is the input itself, for submissions small enough to be worth
	// carrying: a locus list is a few hundred bytes and a round trip through
	// storage would cost more than it saves.
	//
	// Exactly one of Body and InputURI is set. The database enforces it, because
	// neither is a job that can be claimed and then cannot run, and both is two
	// inputs with no rule about which wins.
	Body []byte
	// InputURI locates the input in job storage, for submissions that should not
	// pass through this process whole — an uploaded VCF above all. See
	// migration 0032.
	InputURI string
}

// How a job was submitted. Empty is a third state, not a default: rows written
// before this was recorded genuinely do not say, and counting them as either
// would put a number on a distinction nobody captured.
const (
	OriginWeb = "web"
	OriginAPI = "api"
)

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
	`COALESCE(n_variants,0), client_ip, session_id, COALESCE(user_id,''), label, weight, ` +
	`COALESCE(origin,''), COALESCE(max_variants,0), created_at, ` +
	`COALESCE(started_at,0), COALESCE(finished_at,0)`

// jobColsJ is jobCols qualified with the "j" alias, for the claim query's join.
const jobColsJ = `j.id, j.kind, j.snapshot, j.selection, j.status, COALESCE(j.error,''), ` +
	`COALESCE(j.n_variants,0), j.client_ip, j.session_id, COALESCE(j.user_id,''), j.label, j.weight, ` +
	`COALESCE(j.origin,''), COALESCE(j.max_variants,0), j.created_at, ` +
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
	// VCFURI is the job's answer already assembled as a VCF, when the worker
	// was able to build one. Empty for a locus job, which has no submitted file
	// to merge onto, and for a merge that did not succeed — in both cases the
	// export renders from rows instead.
	VCFURI string
}

// Runner annotates one job's input. An error marks the job failed (its message
// is stored on the job).
type Runner func(ctx context.Context, job Job, input []byte) (Outcome, error)

// Queue is the job queue and its worker pool.
type Queue struct {
	pool  *pgxpool.Pool
	nowFn func() int64

	// disposeObjects removes a collected job's stored files. Nil in a process
	// that has no storage configured; see SetObjectDisposer.
	disposeObjects func(ctx context.Context, uris []string)

	maxJobsPerIP int // per-IP concurrent running-job cap (<=0 = unlimited)
	// slots is the pool's total capacity in job weight, not job count. A job
	// runs only when the running set's weight plus its own fits. <=0 disables
	// the check, which is the pre-weight behaviour.
	slots int

	wg sync.WaitGroup

	mu     sync.Mutex
	queued chan struct{} // wakes one worker (cap 1, non-blocking send)
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

// New wraps an existing pool, matching how the catalog and the annotation cache
// are built. The queue used to keep the DSN it was opened with, so a caller
// could open a second connection to the same database; nothing has needed that
// since the notification listener started borrowing from the pool instead.
//
// Jobs left running by a crashed worker are not touched here: see
// ReclaimExpired, and the comment on it for why recovering on open was wrong.
//
// The caller must have applied migrations/ first; nothing here creates schema.
func New(pool *pgxpool.Pool) (*Queue, error) {
	id, err := NewID()
	if err != nil {
		return nil, fmt.Errorf("worker id: %w", err)
	}
	return &Queue{
		pool:       pool,
		nowFn:      func() int64 { return time.Now().Unix() },
		queued:     make(chan struct{}, 1),
		running:    map[string]*runningJob{},
		workerID:   id,
		leaseTTL:   defaultLeaseTTL,
		leaseRenew: defaultLeaseRenew,
	}, nil
}

// Open connects to Postgres and returns a queue owning that pool.
func Open(ctx context.Context, dsn string) (*Queue, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	q, err := New(pool)
	if err != nil {
		pool.Close()
		return nil, err
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
	// Close the attempts of everything whose lease has lapsed, before either
	// statement below changes the status they are selected by. One statement for
	// both the retrying and the exhausted, because from the attempt's point of
	// view they are the same event: a worker took this job and never came back.
	// Whether the *job* gets another go is a separate decision, recorded on the
	// job.
	//
	// This is the row nothing else preserves. finish() never ran for these — the
	// process that would have called it is gone — so without this write the
	// attempt's worker, its start and how long it survived are lost at the moment
	// the reclaim clears claimed_by.
	if _, err := q.pool.Exec(ctx, `
		UPDATE job_attempt a
		   SET ended_at=$1, outcome=$2
		  FROM job j
		 WHERE a.job_id = j.id AND a.outcome IS NULL
		   AND j.status = $3 AND COALESCE(j.lease_until, 0) < $4`,
		q.nowFn(), OutcomeAbandoned, StatusRunning, q.nowFn()); err != nil {
		return 0, fmt.Errorf("close abandoned attempts: %w", err)
	}
	// Past MaxAttempts the job is failed rather than requeued. Without this a
	// job that kills its worker every run came back forever, and each attempt
	// cost whatever the previous one had produced — the scratch of a killed
	// build is only cleaned up by the process that made it.
	//
	// The error text says what happened, because "attempt 3 of 3" is the part
	// an operator needs and the raw truth ("no worker ever reported on this")
	// is not otherwise visible anywhere.
	// Charged here and nowhere else. An abandoned job's worker died rather than
	// reporting, so finish() never ran for it — but it held a slot for a lease's
	// worth of time on every attempt, and a caller whose jobs keep dying would
	// otherwise retry at everyone else's expense for free.
	//
	// DISTINCT because ON CONFLICT cannot touch the same row twice in one
	// statement, and a caller with several exhausted jobs would otherwise fail
	// the whole sweep.
	if _, err := q.pool.Exec(ctx, `
		WITH failed AS (
		UPDATE job
		   SET status=$1, claimed_by=NULL, lease_until=NULL, finished_at=$4,
		       error=$5
		 WHERE status=$2 AND COALESCE(lease_until, 0) < $3
		   AND attempts >= $6
		RETURNING COALESCE(NULLIF(user_id,''), client_ip) AS who
		)
		INSERT INTO queue_caller (who, last_finished_at)
		SELECT DISTINCT who, $4 FROM failed WHERE who <> ''
		ON CONFLICT (who) DO UPDATE
		   SET last_finished_at = GREATEST(queue_caller.last_finished_at, excluded.last_finished_at)`,
		StatusError, StatusRunning, q.nowFn(), q.nowFn(),
		fmt.Sprintf("abandoned %d times without completing — its worker was killed "+
			"each time rather than reporting a failure; see the job log for what it "+
			"was doing", MaxAttempts),
		MaxAttempts); err != nil {
		return 0, fmt.Errorf("fail exhausted jobs: %w", err)
	}
	// Which jobs are about to be reclaimed, so each can be told in its own log
	// that this happened. Without it an abandoned job carries no record of the
	// abandonment: the worker died without writing anything, and "abandoned 3
	// times" arrives with no times, no workers, and no output.
	// The worker is read here and not after: the reclaim clears claimed_by, so
	// once it has run the identity of the process that died is gone. Without it,
	// "abandoned 3 times" cannot be turned into "worker-7 lost twelve jobs in an
	// hour", which is the question asked when a pod is being OOM-killed — and
	// answering it meant grepping individual job logs for the "starting on
	// worker" line.
	type lost struct{ id, worker string }
	var reclaiming []lost
	if rows, qErr := q.pool.Query(ctx, `
		SELECT id, COALESCE(claimed_by,'') FROM job
		 WHERE status=$1 AND COALESCE(lease_until, 0) < $2 AND attempts < $3`,
		StatusRunning, q.nowFn(), MaxAttempts); qErr == nil {
		for rows.Next() {
			var l lost
			if rows.Scan(&l.id, &l.worker) == nil {
				reclaiming = append(reclaiming, l)
			}
		}
		rows.Close()
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
	for _, l := range reclaiming {
		who := l.worker
		if who == "" {
			who = "its worker"
		}
		_ = q.AppendLog(ctx, l.id, "··· "+who+" stopped renewing the lease; "+
			"requeued for another attempt. The process was killed rather than "+
			"failing, so nothing above this line is its explanation.\n")
		// Also to the process log, where it can be correlated with pod restarts.
		// A job's own log answers "what happened to this job"; this answers "is
		// one worker losing all of them", which is the question when a container
		// is being OOM-killed and the kill itself is invisible from in here.
		log.Printf("queue: job %s abandoned by %s; requeued", l.id, who)
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

// SetObjectDisposer supplies what removes a job's stored files when the job
// itself is collected.
//
// A hook rather than a storage client held here, because this package is about
// job persistence and knows nothing about buckets — the same separation that
// keeps the runner from having one. Unset, expiring jobs still have their rows
// removed and their objects are left for a listing sweep to find.
func (q *Queue) SetObjectDisposer(f func(ctx context.Context, uris []string)) {
	q.disposeObjects = f
}

// SetSlots sets the pool's capacity in job weight.
//
// Separate from the worker count on purpose: the goroutines decide how many
// jobs can be in flight at all, this decides how much work they may hold. A
// deployment that wants annotations to keep flowing during a provisioning run
// gives itself more slots than a download weighs.
func (q *Queue) SetSlots(n int) { q.slots = n }

// NewID returns a random 128-bit hex id.
//
// Exported so a caller can mint one *before* enqueuing, which is what lets an
// uploaded input be stored under the id of the job that will read it. Without
// that the object would have to be written under some other name and the two
// reconciled afterwards, and "which objects belong to jobs that no longer
// exist" would need a join instead of a listing.
//
// This is for internal callers — the API handler that accepts an upload. It is
// not a hook for letting a client choose its own job id: that would hand out
// collisions with other people's jobs, an enumerable id space, and the ability
// to claim an id before its owner does.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// idPattern is what NewID produces, and the only shape Enqueue accepts.
//
// Checked rather than trusted because a job id becomes a path segment in job
// storage. An id containing a slash or a "…/../…" would place the object
// somewhere other than the prefix it was meant for — silently, since writing to
// a valid-looking key succeeds. Every other reason to validate is secondary to
// that one.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Enqueue records a new queued job (metadata + its input) and wakes a worker.
func (q *Queue) Enqueue(ctx context.Context, j NewJob) (string, error) {
	id := j.ID
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return "", err
		}
	} else if !idPattern.MatchString(id) {
		return "", fmt.Errorf("job id %q is not one NewID produced", id)
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(ctx,
		`INSERT INTO job (id,kind,snapshot,selection,status,client_ip,session_id,user_id,label,
		                  weight,max_concurrent,origin,max_variants,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		id, j.Kind, j.Snapshot, j.Selection, StatusQueued,
		j.ClientIP, j.Session, j.UserID, j.Label, weightOf(j.Weight),
		j.MaxConcurrent, j.Origin, j.MaxVariants, q.nowFn()); err != nil {
		return "", err
	}
	// One of the two, never both — matching the CHECK rather than trusting it.
	// A URI wins when set, so a caller that filled in both by mistake gets the
	// one it went to the trouble of uploading rather than a stale body.
	var body, uri any
	if j.InputURI != "" {
		uri = j.InputURI
	} else {
		body = j.Body
		if j.Body == nil {
			// An empty body is still a body: NULL would trip the constraint and
			// fail the insert with a message about neither column being set,
			// which says nothing about the empty submission that caused it.
			body = []byte{}
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO job_input (job_id,body,uri) VALUES ($1,$2,$3)`, id, body, uri); err != nil {
		return "", err
	}
	// NOTIFY fires on commit, so a listener never sees a job it cannot yet claim.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1,$2)`, chanQueued, id); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	where := fmt.Sprintf("%d bytes", len(j.Body))
	if j.InputURI != "" {
		where = "input at " + j.InputURI
	}
	log.Printf("queue: job %s queued (kind=%s, ip=%s, session=%s, selection=%q, %s)",
		id, j.Kind, j.ClientIP, j.Session, j.Selection, where)
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
// Input returns the body a job was submitted with, for the jobs that carry one.
//
// Retained rather than discarded once the job runs, which is what lets a VCF
// submission be answered with its own file annotated instead of a synthesised
// one carrying only the columns this server knows about.
//
// ok is false for a job whose input is in storage. That is not an error and
// callers must not treat it as one: it is the ordinary case for anything large,
// and the point of storing it there is that this process never holds it. Use
// InputRef and stream it.
func (q *Queue) Input(ctx context.Context, id string) ([]byte, bool, error) {
	var body []byte
	err := q.pool.QueryRow(ctx,
		`SELECT body FROM job_input WHERE job_id=$1 AND body IS NOT NULL`, id).Scan(&body)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return body, true, nil
}

// KnownJobIDs returns every job id the database still has a row for.
//
// For the storage sweep, which decides what to delete by what is *absent* — so
// this has to be the whole set, not a page of it. A job whose id is missing from
// a partial answer would have its files collected while it was still queued.
//
// The whole table, because a job's files are owned for as long as its row
// exists, whatever state it is in. Filtering to terminal jobs here would delete
// the input of everything currently queued.
func (q *Queue) KnownJobIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := q.pool.Query(ctx, `SELECT id FROM job`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// TryLock takes a session-scoped advisory lock, reporting whether it got it.
//
// For work that should happen on one replica at a time and can simply be
// skipped by the others — a full bucket listing run by three workers is three
// listings for one answer. Non-blocking on purpose: the loser has nothing to
// wait for, since by the time the winner is done the work is done.
//
// The returned release must be called; it is a no-op when the lock was not
// taken.
func (q *Queue) TryLock(ctx context.Context, name string) (ok bool, release func(), err error) {
	conn, err := q.pool.Acquire(ctx)
	if err != nil {
		return false, func() {}, err
	}
	// One connection held for the lock's lifetime: an advisory lock belongs to a
	// session, and a pool is free to hand the unlock to a different one.
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext($1))`, name).Scan(&ok); err != nil {
		conn.Release()
		return false, func() {}, err
	}
	if !ok {
		conn.Release()
		return false, func() {}, nil
	}
	return true, func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext($1))`, name)
		conn.Release()
	}, nil
}

// ResultVCF returns where a job's answer-as-a-VCF is stored, and whether one
// was built at all.
//
// False is the ordinary case, not a failure: a locus job has no submitted file
// to merge onto, and a job that finished before this existed has no object. The
// caller renders from rows instead.
func (q *Queue) ResultVCF(ctx context.Context, id string) (string, bool, error) {
	var uri *string
	err := q.pool.QueryRow(ctx, `SELECT vcf_uri FROM job_result WHERE job_id=$1`, id).Scan(&uri)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if uri == nil || *uri == "" {
		return "", false, nil
	}
	return *uri, true, nil
}

// InputRef returns where a job's input is stored, and whether it is stored at
// all rather than carried inline.
func (q *Queue) InputRef(ctx context.Context, id string) (string, bool, error) {
	var uri *string
	err := q.pool.QueryRow(ctx, `SELECT uri FROM job_input WHERE job_id=$1`, id).Scan(&uri)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if uri == nil || *uri == "" {
		return "", false, nil
	}
	return *uri, true, nil
}

// DropInput deletes a job's input row, and reports the storage URI it referred
// to so the caller can delete the object too.
//
// Two steps rather than one, and deliberately not transactional: an object left
// behind after the row is gone is scrap that the storage sweep collects, while a
// row pointing at an object that has been deleted is a job that looks runnable
// and is not. Losing the pointer last is the safe order.
func (q *Queue) DropInput(ctx context.Context, id string) (string, error) {
	var uri *string
	err := q.pool.QueryRow(ctx,
		`DELETE FROM job_input WHERE job_id=$1 RETURNING uri`, id).Scan(&uri)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if uri == nil {
		return "", nil
	}
	return *uri, nil
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
		&j.Origin, &j.MaxVariants, &j.CreatedAt, &j.StartedAt, &j.FinishedAt); err != nil {
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
	// The objects these jobs own — inputs and built results alike — read before
	// their rows go.
	//
	// job_input cascades from job, so deleting the row destroys the only record
	// of where the object was — and an object nothing points at is invisible to
	// everything short of listing the whole bucket. Without this, every VCF job
	// that ages out leaves its input behind for good, and storage grows without
	// limit while the table it was tracked in shrinks on schedule.
	var owned []string
	if rows, err := q.pool.Query(ctx, `
		SELECT i.uri FROM job_input i JOIN job j ON j.id = i.job_id
		 WHERE i.uri IS NOT NULL
		   AND j.finished_at IS NOT NULL AND j.finished_at < $1
		UNION ALL
		SELECT r.vcf_uri FROM job_result r JOIN job j ON j.id = r.job_id
		 WHERE r.vcf_uri IS NOT NULL
		   AND j.finished_at IS NOT NULL AND j.finished_at < $1`, cutoff); err == nil {
		for rows.Next() {
			var uri string
			if rows.Scan(&uri) == nil && uri != "" {
				owned = append(owned, uri)
			}
		}
		rows.Close()
	} else {
		// Not fatal: collecting the jobs matters more than collecting their
		// objects, and a leaked object is recoverable by a listing while a table
		// that never shrinks is not.
		log.Printf("queue: could not list the objects of expiring jobs: %v", err)
	}

	tag, err := q.pool.Exec(ctx,
		`DELETE FROM job WHERE finished_at IS NOT NULL AND finished_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	// After the rows, deliberately. This order can leak an object when the
	// disposal fails; the other order can leave a job that still looks complete
	// with its input already gone, which is a wrong answer rather than waste.
	if len(owned) > 0 && q.disposeObjects != nil {
		q.disposeObjects(ctx, owned)
	}

	// The fair-share timestamps age out with the jobs. Safe because the ordering
	// reads GREATEST(created_at, last_finished_at): once the timestamp is older
	// than any queued job could be, it never wins that comparison, so an absent
	// row and a sufficiently old one give the same answer. Without this the table
	// grows a row per anonymous address forever.
	//
	// Best effort — this is housekeeping, and failing the sweep over it would
	// leave the jobs themselves uncollected.
	if _, err := q.pool.Exec(ctx,
		`DELETE FROM queue_caller WHERE last_finished_at < $1`, cutoff); err != nil {
		log.Printf("queue: prune fair-share timestamps: %v", err)
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
// Fairness has two terms, covering two different ways one caller can crowd out
// another:
//
//   - fewest jobs *running* — stops one caller holding every slot at once.
//   - longest since they were *served* — stops one caller taking every slot in
//     sequence. A job's position is GREATEST(created_at, last_finished_at), so
//     finishing a job pushes the rest of that caller's queue behind everyone who
//     has been waiting.
//
// The second exists because the first is blind at one slot. By the time a worker
// claims, the job it just finished is already marked done, so with slots=1 every
// caller has zero running, the term is a constant, and the ordering collapses to
// plain FIFO — a caller who queued 400 jobs before anyone else arrived took all
// 400 in a row. More generally a concurrency-based signal can only separate
// callers up to the number of slots, and has nothing to say when there is one.
//
// Both are kept because each is blind where the other sees: last_finished_at only
// advances on completion, so a caller with a six-hour job in flight looks idle by
// that measure, and only the running count stops them taking more slots.
//
// Deliberately no DISTINCT ON to take one candidate per caller. It would change
// nothing — LIMIT 1 already means a caller's 400 tied rows cannot outvote another
// caller's one — and FOR UPDATE cannot be combined with DISTINCT, so it would
// force the whole statement into CTEs and put the locking, which is the part with
// teeth, on new ground for no gain.
//
// Skip any caller at the per-caller cap, and break ties by oldest created_at then
// id.
//
// The timestamps are Unix seconds, so callers finishing inside the same second
// tie and that burst drains FIFO. Deliberate: jobs completing that fast mean the
// queue is not contended, so there is nothing for fairness to protect, and the
// next tick resolves it. Finer resolution would buy ordering nobody is waiting
// on, at the cost of widening three columns and everything that reads them.
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
    SELECT COALESCE(NULLIF(user_id,''), client_ip) AS who, COUNT(*) AS c
      FROM job WHERE status = $1
     GROUP BY COALESCE(NULLIF(user_id,''), client_ip)
  ) r ON r.who = COALESCE(NULLIF(j.user_id,''), j.client_ip)
  LEFT JOIN queue_caller f
    ON f.who = COALESCE(NULLIF(j.user_id,''), j.client_ip)
  CROSS JOIN (
    SELECT COALESCE(SUM(weight),0) AS used FROM job WHERE status = $1
  ) p
  WHERE j.status = $3
    AND COALESCE(r.c, 0) < (CASE WHEN j.max_concurrent > 0 THEN j.max_concurrent ELSE $4 END)
    AND (p.used = 0 OR p.used + j.weight <= $5)
  ORDER BY COALESCE(r.c, 0) ASC,
           GREATEST(j.created_at, COALESCE(f.last_finished_at, 0)) ASC,
           j.created_at ASC, j.id ASC
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
	// Open this attempt's history row in the claim transaction, so a claimed job
	// always has one. Written apart, a crash in between would leave an attempt
	// that ran and was never recorded — and the rows this table exists for are
	// exactly the ones whose worker did not survive to write anything later.
	//
	// The attempt number is read back from the row the UPDATE just incremented
	// rather than counted here, which is what keeps it equal to job.attempts
	// instead of merely close to it.
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_attempt (job_id, attempt, worker, started_at)
		SELECT id, attempts, $2, $3 FROM job WHERE id = $1
		ON CONFLICT (job_id, attempt) DO NOTHING`,
		job.ID, q.workerID, q.nowFn()); err != nil {
		return Job{}, nil, false, err
	}
	// Read the input inside the same transaction, so a claim and its input are
	// one atomic step: committing the claim and then failing to read it would
	// leave a job marked running that no worker is running.
	//
	// A stored input yields a URI and no bytes. The claim only needs to know it
	// exists — staging it is the runner's job, and doing it here would hold the
	// claim transaction open for the length of a download.
	var body []byte
	var uri *string
	if err := tx.QueryRow(ctx,
		`SELECT body, uri FROM job_input WHERE job_id=$1`, job.ID).Scan(&body, &uri); err != nil {
		return Job{}, nil, false, err
	}
	if uri != nil {
		job.InputURI = *uri
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
			`INSERT INTO job_result (job_id,json,vcf_uri) VALUES ($1,$2,$3)
			 ON CONFLICT (job_id) DO UPDATE
			    SET json = excluded.json, vcf_uri = excluded.vcf_uri`,
			id, string(out.Result), nullable(out.VCFURI)); err != nil {
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
	// Close this job's open attempt with the same outcome, in the same
	// transaction. Identified by "the one still open" rather than by number,
	// because finish() is reached from paths that never saw the claim — and
	// there is at most one, since claiming opens exactly one and every terminal
	// path closes it.
	//
	// No row is a normal case, not an error: a queued job cancelled before any
	// worker took it never had an attempt.
	if _, err := tx.Exec(wctx, `
		UPDATE job_attempt SET ended_at=$2, outcome=$3, error=$4
		 WHERE job_id=$1 AND outcome IS NULL`,
		id, q.nowFn(), status, errArg); err != nil {
		log.Printf("queue: close attempt for job %s: %v", id, err)
		return
	}
	// In the same transaction as the finish, so the scheduler can never see a job
	// completed without the charge for it having landed. Apart, a crash between
	// the two would leave that caller's next job holding a position it has
	// already spent.
	if err := chargeCaller(wctx, tx, id, q.nowFn()); err != nil {
		log.Printf("queue: charge caller for job %s: %v", id, err)
		return
	}
	if err := tx.Commit(wctx); err != nil {
		log.Printf("queue: commit job %s: %v", id, err)
		return
	}
}

// nullable renders an empty string as SQL NULL.
//
// A column that means "there is no such thing" should hold NULL rather than "",
// so a reader can ask IS NULL instead of knowing that empty is the sentinel.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// chargeCaller records that a job's caller has just been served, which pushes the
// rest of their queue behind everyone who has been waiting.
//
// The identity is derived from the job row in SQL rather than from a Go field,
// so it is the same expression the claim query orders by. Written twice, the two
// could disagree — and a job charged to one identity while ordered under another
// is a scheduler that quietly stops being fair.
//
// GREATEST, not assignment: a cancel and a completion can land out of order, and
// moving the timestamp backwards would hand back a turn that was already taken.
//
// A job with neither an account nor an address has no caller to be fair between,
// so the WHERE drops it rather than filing everything anonymous under one key.
func chargeCaller(ctx context.Context, tx pgx.Tx, id string, now int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO queue_caller (who, last_finished_at)
		SELECT COALESCE(NULLIF(user_id,''), client_ip), $2
		  FROM job
		 WHERE id = $1 AND COALESCE(NULLIF(user_id,''), client_ip) <> ''
		ON CONFLICT (who) DO UPDATE
		   SET last_finished_at = GREATEST(queue_caller.last_finished_at, excluded.last_finished_at)`,
		id, now)
	return err
}

// WorkerID identifies this process in the claims it holds, for logs that need
// to say which worker did something.
func (q *Queue) WorkerID() string { return q.workerID }

// AppendLog adds to a job's recorded output without reading it back first.
//
// Concatenated in SQL rather than read-modify-written, so two writers — the
// worker streaming its run, and a peer noting that the job was abandoned —
// cannot lose each other's lines.
func (q *Queue) AppendLog(ctx context.Context, id, s string) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO job_log (job_id, output) VALUES ($1,$2)
		ON CONFLICT (job_id) DO UPDATE SET output = job_log.output || EXCLUDED.output`,
		id, s)
	return err
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
