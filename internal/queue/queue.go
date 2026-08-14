// Package queue is the Postgres-backed async chunk queue: it persists chunks,
// their inputs, and their results, and drives a worker pool that annotates
// queued chunks.
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
//     request, is not. NOTIFY also means a waiter in one replica learns about
//     a chunk finished by a worker in another.
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

// Chunk statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusError   = "error"
	// StatusCancelled is a chunk someone stopped on purpose.
	//
	// Its own status rather than an error with a message: a cancel is a
	// decision, not a fault, and counting one as a failure would make a
	// deliberate stop indistinguishable from something going wrong — on the
	// metrics page most of all, where the failure rate is what gets watched.
	StatusCancelled = "cancelled"
)

// How one attempt at a chunk ended, in chunk_attempt.outcome.
//
// Three of the four are the chunk statuses, deliberately: an attempt that
// finished is an attempt that put the chunk into that state, and giving them
// separate spellings would mean two vocabularies for one fact.
//
// The fourth has no status because it is not something a chunk can be. A chunk
// abandoned twice and then completed is a done chunk; only the attempts
// remember that anything went wrong, which is the whole reason they are
// recorded.
const (
	OutcomeDone      = StatusDone
	OutcomeError     = StatusError
	OutcomeCancelled = StatusCancelled
	// OutcomeAbandoned is an attempt whose worker stopped renewing the lease —
	// killed rather than failing, so it never reported anything.
	OutcomeAbandoned = "abandoned"
)

// Chunk kinds.
const (
	KindLocus = "locus"
	KindVCF   = "vcf"
	// KindDownload provisions a snapshot's source data instead of annotating. It
	// shares the queue so it gets the same persistence, scheduling and error
	// reporting; the worker dispatches on this.
	KindDownload = "download"
	// KindCleanup reclaims a removed source's files, for the same reason
	// downloads are chunks: only the worker mounts the storage.
	KindCleanup = "cleanup"
	// KindMove relocates a source's files between storage locations. A chunk
	// because it moves the same volume of data a download does, and because
	// only the worker can reach both ends.
	KindMove = "move"
	// KindSplit cuts a submitted VCF into pieces, and queues a chunk for each
	// piece plus the join that will put them back together.
	//
	// A chunk rather than work done in the request handler: cutting a
	// chromosome means reading hundreds of megabytes and writing them back,
	// which is a worker's business. It is also the first chunk of the job, so
	// it is what runs while a caller is polling and seeing nothing yet.
	KindSplit = "split"
	// KindCollect joins a job's annotated pieces into the final file.
	//
	// Queued by the split, at the same time as the pieces, and held back until
	// they are done — see Chunk.AwaitsPieces. Queued by whichever piece
	// finished last, the work a job still owed was an intention held by a
	// worker rather than a row anything could see.
	KindCollect = "collect"
)

// Postgres NOTIFY channels.
const (
	chanQueued = "chunk_queued"
	// chanCancel carries a chunk id whose run should stop. The worker holding
	// it may be in another process, which is the whole reason this goes
	// through the database rather than a method call.
	chanCancel = "chunk_cancel"
)

// Chunk is one row of the chunk table (its metadata, without the input/result
// blobs): the unit a worker claims, leases, retries and abandons.
//
// It is still what the published API calls a job, and its id is still
// serialized as job_id. That is not an oversight and not a transitional state:
// the id a submitter is given is a chunk id — the split chunk's, for a split
// submission — and renaming the field would break every client for no gain. A
// Job here is the submission those chunks belong to, which callers reach
// through the same /jobs/{id} they always have.
type Chunk struct {
	ID        string `json:"job_id" doc:"Stable identifier. Poll and fetch results with it."`
	Kind      string `json:"kind" doc:"locus | vcf for annotation; download | move for provisioning."`
	Snapshot  string `json:"snapshot" doc:"The snapshot annotated against. An individual-source selection becomes a generated snapshot, which is what makes the run reproducible."`
	Selection string `json:"selection" doc:"The annotation fields asked for, or empty for the snapshot's defaults."`
	Status    string `json:"status" doc:"queued | running | done | error | cancelled."`
	Error     string `json:"error,omitempty" doc:"Why the chunk failed, when it did."`
	NVariants int64  `json:"n_variants" doc:"How many variants were annotated."`
	ClientIP  string `json:"client_ip,omitempty" doc:"The address the chunk was submitted from."`
	Session   string `json:"session,omitempty" doc:"The submitter's session, which scopes an anonymous caller's own history."`
	UserID    string `json:"user_id,omitempty" doc:"The owning account, when the submitter had one. Authoritative where session is not, being written from the credential the server verified."`
	Label     string `json:"label,omitempty" doc:"A short human label: the locus, or the submitted filename."`

	Weight     int    `json:"weight,omitempty" doc:"How many worker slots this chunk occupies while it runs. An annotation is 1; provisioning is heavier, because it saturates disk and CPU for hours and two at once finish later than one after the other."`
	Origin     string `json:"origin,omitempty" doc:"How the chunk was submitted: \"web\" from a browser session, \"api\" from a personal access token. Absent for chunks recorded before this was tracked."`
	CreatedAt  int64  `json:"created_at" doc:"Unix seconds."`
	StartedAt  int64  `json:"started_at,omitempty" doc:"Unix seconds. Absent until a worker claims it."`
	FinishedAt int64  `json:"finished_at,omitempty" doc:"Unix seconds. Absent until it finishes."`

	// MaxVariants is the variant cap this chunk was admitted under; 0 is
	// unlimited. Stamped at submit, so the terms a chunk runs under are the
	// ones that applied when it was accepted.
	//
	// Not serialized: it is an internal admission decision, and a caller who
	// wants to know their limit asks about their account rather than reading
	// it off somebody's chunk.
	MaxVariants int `json:"-"`

	// JobID names the split submission this chunk belongs to, and ChunkIndex
	// its place in it. Empty and nil for an ordinary chunk.
	//
	// Not serialized here: what a caller needs is the job's progress, which
	// GET /jobs/{id} answers, rather than an internal id hanging off every
	// chunk in a list.
	JobID      string `json:"-"`
	ChunkIndex *int   `json:"-"`

	// AwaitsPieces holds this chunk back until every piece of its job is done.
	//
	// The collect that joins a split submission, and nothing else. It is queued
	// with the pieces rather than by whichever of them finishes last, so what a
	// job still owes is visible as rows — see migration 0035, and the claim
	// query's predicate.
	AwaitsPieces bool `json:"-"`

	// CompletesJob marks the chunk whose stored output is the job's answer:
	// the only chunk of an unsplit submission, or the collect of a split one.
	//
	// Which chunk to serve, not when the job is done — that is every chunk
	// being done. Recorded when the chunk is created because the only code that
	// knows is the code that queued it.
	CompletesJob bool `json:"-"`

	// InputURI is where this chunk's input is stored, set only on the Chunk a
	// claim returns — Get and List do not fill it, because nothing reading a
	// chunk's status needs it.
	//
	// Never serialized. It names a bucket and key inside the deployment, which
	// is an operator's business and not something a chunk status response
	// should hand to whoever asks. The rest of this struct is the published
	// API; this is not part of it.
	InputURI string `json:"-"`
}

// Terminal reports whether the chunk has reached a final status.
func (j Chunk) Terminal() bool {
	return j.Status == StatusDone || j.Status == StatusError || j.Status == StatusCancelled
}

// NewChunk is the metadata for enqueuing a chunk (plus its input).
type NewChunk struct {
	// ID is the chunk's identifier, minted by NewID. Empty means Enqueue mints
	// one, which is what every caller that has no reason to care does.
	//
	// Supplied only by a caller that had to name the chunk before creating it
	// — an upload writes its object to jobs/<id>/ so a bucket listing says
	// which chunk each object belongs to. Never taken from a request: see
	// NewID.
	ID        string
	Kind      string
	Snapshot  string
	Selection string
	ClientIP  string
	Session   string
	UserID    string
	Label     string
	// Weight is how much of the pool this chunk occupies; 0 means 1.
	Weight int
	// MaxConcurrent caps how many of this submitter's chunks may run at once.
	// 0 falls back to the deployment's per-IP cap, which is what anonymous
	// work and anything submitted before tiers existed gets.
	//
	// Recorded on the row rather than read from the account at dispatch: the
	// claim query stays one statement over one table, a chunk keeps the terms
	// it was admitted under, and an anonymous chunk has no account to read
	// from.
	MaxConcurrent int
	// Origin is how the chunk arrived: OriginWeb, OriginAPI, or empty when
	// unrecorded. Reporting only — it decides nothing.
	Origin string
	// MaxVariants caps how many variants this chunk may carry; 0 is unlimited.
	// Recorded on the row so the worker enforcing it does not have to resolve
	// an account that may not exist.
	MaxVariants int
	// JobID and ChunkIndex place this chunk in a split submission. ChunkIndex
	// is a pointer because 0 is a real chunk — the one carrying the header —
	// and "not a chunk" has to be distinguishable from "the first chunk".
	JobID      string
	ChunkIndex *int
	// AwaitsPieces and CompletesJob place this chunk in a fan-out. See the
	// fields of the same name on Chunk.
	AwaitsPieces bool
	CompletesJob bool

	// Body is the input itself, for submissions small enough to be worth
	// carrying: a locus list is a few hundred bytes and a round trip through
	// storage would cost more than it saves.
	//
	// Exactly one of Body and InputURI is set. The database enforces it,
	// because neither is a chunk that can be claimed and then cannot run, and
	// both is two inputs with no rule about which wins.
	Body []byte
	// InputURI locates the input in job storage, for submissions that should not
	// pass through this process whole — an uploaded VCF above all. See
	// migration 0032.
	InputURI string
}

// How a chunk was submitted. Empty is a third state, not a default: rows
// written before this was recorded genuinely do not say, and counting them as
// either would put a number on a distinction nobody captured.
const (
	OriginWeb = "web"
	OriginAPI = "api"
)

// weightOf normalizes a chunk's weight. 0 means the caller did not care, which
// is one slot — the same as an annotation.
func weightOf(w int) int {
	if w < 1 {
		return 1
	}
	return w
}

// chunkCols is the SELECT list backing scanChunk. NULLable columns are
// coalesced in SQL so the scan targets are plain Go values — the SQLite port
// used sql.NullXxx and then threw the validity flag away, which amounts to the
// same thing with more ceremony.
const chunkCols = `id, kind, snapshot, selection, status, COALESCE(error,''), ` +
	`COALESCE(n_variants,0), client_ip, session_id, COALESCE(user_id,''), label, weight, ` +
	`COALESCE(origin,''), COALESCE(max_variants,0), COALESCE(job_id,''), chunk_index, ` +
	`awaits_pieces, completes_job, created_at, COALESCE(started_at,0), ` +
	`COALESCE(finished_at,0)`

// chunkColsJ is chunkCols qualified with the "j" alias, for the claim query's
// join.
const chunkColsJ = `j.id, j.kind, j.snapshot, j.selection, j.status, COALESCE(j.error,''), ` +
	`COALESCE(j.n_variants,0), j.client_ip, j.session_id, COALESCE(j.user_id,''), j.label, j.weight, ` +
	`COALESCE(j.origin,''), COALESCE(j.max_variants,0), COALESCE(j.job_id,''), j.chunk_index, ` +
	`j.awaits_pieces, j.completes_job, j.created_at, COALESCE(j.started_at,0), ` +
	`COALESCE(j.finished_at,0)`

// ErrNotCancellable is returned when a chunk has already finished.
var ErrNotCancellable = errors.New("chunk is not running")

// ErrNoSuchChunk is returned for an unknown id.
var ErrNoSuchChunk = errors.New("no such chunk")

// cancelChunk stops one chunk.
//
// A queued chunk is settled here and never starts. A running one is signalled
// over NOTIFY, because the worker executing it is usually in another process —
// that is the whole reason this goes through the database rather than a method
// call. Its worker records the outcome, so a cancel does not race the run to
// write the row.
//
// Unexported: a caller cancels a job, and CancelJob does this to each chunk of
// it. Cancelling one chunk of a split submission on its own would leave a job
// that cannot finish and cannot be joined, with nothing saying why.
func (q *Queue) cancelChunk(ctx context.Context, id string) (Chunk, error) {
	// Settle it here if it has not started: no worker is involved, so there is
	// nothing to signal and nothing to wait for.
	row := q.pool.QueryRow(ctx, `
		UPDATE chunk SET status=$2, error='cancelled', finished_at=$3
		 WHERE id=$1 AND status=$4
		RETURNING `+chunkCols, id, StatusCancelled, q.nowFn(), StatusQueued)
	chunk, err := scanChunk(row)
	if err == nil {
		return chunk, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Chunk{}, err
	}

	chunk, ok, err := q.Get(ctx, id)
	if err != nil {
		return Chunk{}, err
	}
	if !ok {
		return Chunk{}, ErrNoSuchChunk
	}
	if chunk.Status != StatusRunning {
		return chunk, ErrNotCancellable
	}
	if _, err := q.pool.Exec(ctx, `SELECT pg_notify($1,$2)`, chanCancel, id); err != nil {
		return Chunk{}, err
	}
	return chunk, nil
}

// SetLog records what a chunk's run printed.
//
// Written separately from the outcome and best-effort at the call site: a log
// that fails to store must not turn a successful chunk into a failed one, and
// a failed chunk's log is worth keeping even when the failure itself is what
// is being recorded.
func (q *Queue) SetLog(ctx context.Context, id, output string) error {
	if output == "" {
		return nil
	}
	_, err := q.pool.Exec(ctx, `
		INSERT INTO chunk_log (chunk_id, output) VALUES ($1,$2)
		ON CONFLICT (chunk_id) DO UPDATE SET output = excluded.output`, id, output)
	return err
}

// Log returns what a job's own run printed, and whether anything was recorded.
//
// The job's first chunk: the run a caller submitted, or — for a split — the
// split itself, which is the one that says how the file was cut and what went
// wrong if it could not be. Each piece keeps its own log, reachable at
// /jobs/{id}/chunks/{chunkID}/log; concatenating them here would make a status
// page download twenty-six logs to show one.
func (q *Queue) Log(ctx context.Context, jobID string) (string, bool, error) {
	var out string
	err := q.pool.QueryRow(ctx, `
		SELECT output FROM chunk_log
		 WHERE chunk_id = (SELECT input_chunk_id FROM job WHERE id=$1)`, jobID).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// Outcome is what a Runner produces for a completed chunk.
type Outcome struct {
	// Result is the annotation JSON the CLI emitted. It is projected into
	// chunk_variant and written out as the stored VCF, and is not itself
	// stored: for 2.6M variants it is gigabytes of JSON in one column, and
	// nothing has read it since chunk_variant existed to be paged instead.
	Result []byte
	// N is the number of variants.
	N int
	// Columns is the JSON column model for these results (may be nil). Stored
	// on the chunk so its results stay renderable even if the snapshot is
	// re-pinned.
	Columns []byte
	// Variants says whether Result is an annotated-variant array that should
	// be projected into chunk_variant. A download's result is a file manifest.
	Variants bool
	// MaxRows bounds how many variants are kept as rows; 0 keeps every one.
	//
	// Carried on the outcome because the number is a deployment setting the
	// worker resolves per chunk, while which chunk may contribute rows is a fact
	// about the job's shape that the queue knows. Each decides the half it can
	// see.
	MaxRows int
	// VCFURI is where the chunk's answer was stored as a VCF. Every annotation
	// chunk produces one — a submitted file with the annotations set on its
	// records, or a sites-only render for a locus list — and it is what every
	// export is a conversion of. Empty when the build did not succeed, and then
	// the export renders from rows instead.
	VCFURI string
}

// Runner annotates one chunk's input. An error marks the chunk failed (its
// message is stored on the chunk).
type Runner func(ctx context.Context, chunk Chunk, input []byte) (Outcome, error)

// Queue is the chunk queue and its worker pool.
type Queue struct {
	pool  *pgxpool.Pool
	nowFn func() int64

	// disposeObjects removes a collected chunk's stored files. Nil in a
	// process that has no storage configured; see SetObjectDisposer.
	disposeObjects func(ctx context.Context, uris []string)

	maxJobsPerIP int // per-IP concurrent running-chunk cap (<=0 = unlimited)
	// slots is the pool's total capacity in chunk weight, not chunk count. A
	// chunk runs only when the running set's weight plus its own fits. <=0
	// disables the check, which is the pre-weight behaviour.
	slots int

	wg sync.WaitGroup

	mu     sync.Mutex
	queued chan struct{} // wakes one worker (cap 1, non-blocking send)
	// running tracks chunks this process is executing, so a cancel arriving
	// over NOTIFY can reach the right subprocess. Only ever holds chunks
	// claimed here; a cancel for another replica's chunk finds nothing and is
	// ignored, which is correct — that replica is listening too.
	//
	// It is also exactly the set whose leases this process must renew: a chunk
	// is in here for as long as this process is really working on it.
	running map[string]*runningChunk

	// workerID identifies this process in the claims it holds. Random per
	// process rather than derived from a hostname or pid, both of which a
	// container reuses across restarts — a restarted worker must not look like
	// the one that died, or it would appear to be renewing its predecessor's
	// leases.
	workerID string
	// leaseTTL is how long a claim stays valid without renewal, and leaseRenew
	// how often the holder refreshes it. The gap between them is the margin: a
	// worker has several chances to renew before anything reclaims its work,
	// so a slow query or a pause does not cost it a chunk it is actively
	// running.
	leaseTTL   time.Duration
	leaseRenew time.Duration
}

// Default lease timings. The TTL is generous relative to the renew interval
// because the cost of the two errors is not symmetric: renewing more often
// than needed costs one tiny UPDATE, while expiring a lease a live worker
// still holds takes a chunk away mid-run and lets a second worker start it
// again.
const (
	// Sized for the longest thing a worker actually does, not for the shortest.
	//
	// Two minutes was too tight. A provisioning chunk runs for hours doing
	// heavy I/O, and a node under disk pressure can starve the process past
	// that — after which the holder is still working while a peer reclaims the
	// chunk and starts it again, both writing to one destination. Renewal is
	// cheap and frequent, so the margin here is a dozen missed renewals rather
	// than six.
	defaultLeaseTTL   = 15 * time.Minute
	defaultLeaseRenew = 30 * time.Second
)

// MaxAttempts is how many times a chunk may be handed to a worker before it is
// failed instead of requeued.
//
// Only reached by chunks whose worker died without reporting anything: a chunk
// that fails cleanly is already terminal. Three is enough to ride out a
// rolling restart or a one-off eviction, and few enough that a chunk which
// kills its worker every time stops rather than doing so forever.
const MaxAttempts = 3

// New wraps an existing pool, matching how the catalog and the annotation cache
// are built. The queue used to keep the DSN it was opened with, so a caller
// could open a second connection to the same database; nothing has needed that
// since the notification listener started borrowing from the pool instead.
//
// Chunks left running by a crashed worker are not touched here: see
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
		running:    map[string]*runningChunk{},
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

// ReclaimExpired returns abandoned chunks to the queue, reporting how many.
//
// A chunk is abandoned when its lease has run out: the worker that claimed it
// renews while it works, so nobody renewing means nobody is working on it.
// That is a fact about the chunk rather than about the caller, which is what
// makes this safe to run from any process at any time — including while other
// workers are busy, and including several replicas running it at once.
//
// The predecessor of this was a blanket "requeue everything running" in Open.
// Both the API and the worker open the queue, so restarting the API reset
// chunks underneath the worker still executing them, and a multi-hour download
// could end up running twice against the same destination. The signal there
// was "a process started", which says nothing about whether anyone else is
// working.
//
// A NULL lease is expired: rows claimed before leases existed have nobody
// renewing them either, so they are abandoned by the same definition.
func (q *Queue) ReclaimExpired(ctx context.Context) (int, error) {
	// Close the attempts of everything whose lease has lapsed, before either
	// statement below changes the status they are selected by. One statement
	// for both the retrying and the exhausted, because from the attempt's
	// point of view they are the same event: a worker took this chunk and
	// never came back. Whether the *chunk* gets another go is a separate
	// decision, recorded on the chunk.
	//
	// This is the row nothing else preserves. finish() never ran for these — the
	// process that would have called it is gone — so without this write the
	// attempt's worker, its start and how long it survived are lost at the moment
	// the reclaim clears claimed_by.
	if _, err := q.pool.Exec(ctx, `
		UPDATE chunk_attempt a
		   SET ended_at=$1, outcome=$2
		  FROM chunk j
		 WHERE a.chunk_id = j.id AND a.outcome IS NULL
		   AND j.status = $3 AND COALESCE(j.lease_until, 0) < $4`,
		q.nowFn(), OutcomeAbandoned, StatusRunning, q.nowFn()); err != nil {
		return 0, fmt.Errorf("close abandoned attempts: %w", err)
	}
	// Past MaxAttempts the chunk is failed rather than requeued. Without this
	// a chunk that kills its worker every run came back forever, and each
	// attempt cost whatever the previous one had produced — the scratch of a
	// killed build is only cleaned up by the process that made it.
	//
	// The error text says what happened, because "attempt 3 of 3" is the part
	// an operator needs and the raw truth ("no worker ever reported on this")
	// is not otherwise visible anywhere. Charged here and nowhere else. An
	// abandoned chunk's worker died rather than reporting, so finish() never
	// ran for it — but it held a slot for a lease's worth of time on every
	// attempt, and a caller whose chunks keep dying would otherwise retry at
	// everyone else's expense for free.
	//
	// DISTINCT because ON CONFLICT cannot touch the same row twice in one
	// statement, and a caller with several exhausted chunks would otherwise
	// fail the whole sweep.
	if _, err := q.pool.Exec(ctx, `
		WITH failed AS (
		UPDATE chunk
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
			"each time rather than reporting a failure; see the chunk log for what it "+
			"was doing", MaxAttempts),
		MaxAttempts); err != nil {
		return 0, fmt.Errorf("fail exhausted chunks: %w", err)
	}
	// Which chunks are about to be reclaimed, so each can be told in its own
	// log that this happened. Without it an abandoned chunk carries no record
	// of the abandonment: the worker died without writing anything, and
	// "abandoned 3 times" arrives with no times, no workers, and no output.
	// The worker is read here and not after: the reclaim clears claimed_by, so
	// once it has run the identity of the process that died is gone. Without
	// it, "abandoned 3 times" cannot be turned into "worker-7 lost twelve
	// chunks in an hour", which is the question asked when a pod is being
	// OOM-killed — and answering it meant grepping individual chunk logs for
	// the "starting on worker" line.
	type lost struct{ id, worker string }
	var reclaiming []lost
	if rows, qErr := q.pool.Query(ctx, `
		SELECT id, COALESCE(claimed_by,'') FROM chunk
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
		UPDATE chunk
		   SET status=$1, started_at=NULL, claimed_by=NULL, lease_until=NULL
		 WHERE status=$2 AND COALESCE(lease_until, 0) < $3
		   AND attempts < $4`,
		StatusQueued, StatusRunning, q.nowFn(), MaxAttempts)
	if err != nil {
		return 0, fmt.Errorf("reclaim expired chunks: %w", err)
	}
	for _, l := range reclaiming {
		who := l.worker
		if who == "" {
			who = "its worker"
		}
		_ = q.AppendLog(ctx, l.id, "··· "+who+" stopped renewing the lease; "+
			"requeued for another attempt. The process was killed rather than "+
			"failing, so nothing above this line is its explanation.\n")
		// Also to the process log, where it can be correlated with pod
		// restarts. A chunk's own log answers "what happened to this chunk";
		// this answers "is one worker losing all of them", which is the
		// question when a container is being OOM-killed and the kill itself is
		// invisible from in here.
		log.Printf("queue: chunk %s abandoned by %s; requeued", l.id, who)
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
					log.Printf("queue: reclaimed %d abandoned chunk(s)", n)
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
		UPDATE chunk SET lease_until=$1
		 WHERE id = ANY($2) AND status=$3 AND claimed_by=$4`,
		q.nowFn()+int64(q.leaseTTL.Seconds()), ids, StatusRunning, q.workerID)
	return err
}

// Close releases the connection pool.
func (q *Queue) Close() { q.pool.Close() }

// Ping checks the database is reachable. Used by the readiness probe.
func (q *Queue) Ping(ctx context.Context) error { return q.pool.Ping(ctx) }

// SetMaxJobsPerIP sets the per-IP concurrent running-chunk cap enforced by the
// fair scheduler (<=0 = unlimited). Call before starting workers.
func (q *Queue) SetMaxJobsPerIP(n int) { q.maxJobsPerIP = n }

// SetObjectDisposer supplies what removes a chunk's stored files when the
// chunk itself is collected.
//
// A hook rather than a storage client held here, because this package is about
// chunk persistence and knows nothing about buckets — the same separation that
// keeps the runner from having one. Unset, expiring chunks still have their
// rows removed and their objects are left for a listing sweep to find.
func (q *Queue) SetObjectDisposer(f func(ctx context.Context, uris []string)) {
	q.disposeObjects = f
}

// SetSlots sets the pool's capacity in chunk weight.
//
// Separate from the worker count on purpose: the goroutines decide how many
// chunks can be in flight at all, this decides how much work they may hold. A
// deployment that wants annotations to keep flowing during a provisioning run
// gives itself more slots than a download weighs.
func (q *Queue) SetSlots(n int) { q.slots = n }

// NewID returns a random 128-bit hex id.
//
// Exported so a caller can mint one *before* enqueuing, which is what lets an
// uploaded input be stored under the id of the chunk that will read it.
// Without that the object would have to be written under some other name and
// the two reconciled afterwards, and "which objects belong to chunks that no
// longer exist" would need a join instead of a listing.
//
// This is for internal callers — the API handler that accepts an upload. It is
// not a hook for letting a client choose its own chunk id: that would hand out
// collisions with other people's chunks, an enumerable id space, and the
// ability to claim an id before its owner does.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// idPattern is what NewID produces, and the only shape Submit accepts.
//
// Checked rather than trusted because a job id becomes a path segment in job
// storage. An id containing a slash or a "…/../…" would place the object
// somewhere other than the prefix it was meant for — silently, since writing
// to a valid-looking key succeeds. Every other reason to validate is secondary
// to that one.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Enqueue records a new queued chunk (metadata + its input) and wakes a
// worker.
func (q *Queue) Enqueue(ctx context.Context, j NewChunk) (string, error) {
	id := j.ID
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return "", err
		}
	} else if !idPattern.MatchString(id) {
		return "", fmt.Errorf("chunk id %q is not one NewID produced", id)
	}
	if j.JobID == "" {
		return "", errors.New("a chunk must belong to a job")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	j.ID = id
	if err := insertChunk(ctx, tx, q.nowFn(), j); err != nil {
		return "", err
	}
	// NOTIFY fires on commit, so a listener never sees a chunk it cannot yet
	// claim.
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
	log.Printf("queue: chunk %s queued (kind=%s, ip=%s, session=%s, selection=%q, %s)",
		id, j.Kind, j.ClientIP, j.Session, j.Selection, where)
	q.poke()
	return id, nil
}

// insertChunk writes a chunk row and its input inside an open transaction.
//
// Shared by Submit, which creates a job's first chunk, and Enqueue, which adds
// the rest. One implementation because the row is the same row: two would be
// two column lists to keep in step, and the one that drifts is the one used by
// whichever path is exercised less.
func insertChunk(ctx context.Context, tx pgx.Tx, now int64, j NewChunk) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO chunk (id,kind,snapshot,selection,status,client_ip,session_id,user_id,label,
		                  weight,max_concurrent,origin,max_variants,job_id,chunk_index,
		                  awaits_pieces,completes_job,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		j.ID, j.Kind, j.Snapshot, j.Selection, StatusQueued,
		j.ClientIP, j.Session, j.UserID, j.Label, weightOf(j.Weight),
		j.MaxConcurrent, j.Origin, j.MaxVariants,
		j.JobID, j.ChunkIndex, j.AwaitsPieces, j.CompletesJob, now); err != nil {
		return err
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
	_, err := tx.Exec(ctx,
		`INSERT INTO chunk_input (chunk_id,body,uri) VALUES ($1,$2,$3)`, j.ID, body, uri)
	return err
}

// poke wakes one waiting worker in this process (non-blocking).
func (q *Queue) poke() {
	select {
	case q.queued <- struct{}{}:
	default:
	}
}

// Get returns a chunk's metadata (ok=false when the id is unknown).
func (q *Queue) Get(ctx context.Context, id string) (Chunk, bool, error) {
	row := q.pool.QueryRow(ctx, `SELECT `+chunkCols+` FROM chunk WHERE id=$1`, id)
	j, err := scanChunk(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Chunk{}, false, nil
	}
	if err != nil {
		return Chunk{}, false, err
	}
	return j, true, nil
}

// Result returns a done chunk's result JSON (ok=false when the id is unknown
// or the chunk has no stored result yet). Input returns the body a chunk was
// submitted with, for the chunks that carry one.
//
// Retained rather than discarded once the chunk runs, which is what lets a VCF
// submission be answered with its own file annotated instead of a synthesised
// one carrying only the columns this server knows about.
//
// ok is false for a chunk whose input is in storage. That is not an error and
// callers must not treat it as one: it is the ordinary case for anything
// large, and the point of storing it there is that this process never holds
// it. Use InputRef and stream it.
func (q *Queue) Input(ctx context.Context, jobID string) ([]byte, bool, error) {
	var body []byte
	err := q.pool.QueryRow(ctx, `
		SELECT body FROM chunk_input
		 WHERE chunk_id = (SELECT input_chunk_id FROM job WHERE id=$1)
		   AND body IS NOT NULL`, jobID).Scan(&body)
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
// this has to be the whole set, not a page of it. A job whose id is missing
// from a partial answer would have its files collected while it was still
// queued.
//
// Jobs rather than chunks because storage is laid out by job: everything a
// submission owns — its input, its pieces, their answers, the joined result —
// lives under jobs/<job-id>/. A chunk has no prefix of its own to sweep.
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

// ChunkLog returns what one chunk of a job printed.
func (q *Queue) ChunkLog(ctx context.Context, jobID, chunkID string) (string, bool, error) {
	var out string
	err := q.pool.QueryRow(ctx, `
		SELECT l.output FROM chunk_log l JOIN chunk c ON c.id = l.chunk_id
		 WHERE c.job_id=$1 AND c.id=$2`, jobID, chunkID).Scan(&out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
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
//
// The answer belongs to whichever chunk produced it — the only chunk of an
// unsplit job, the collect of a split one — and the job names it once it is
// done, so this reads through result_chunk_id rather than guessing.
func (q *Queue) ResultVCF(ctx context.Context, jobID string) (string, bool, error) {
	var uri *string
	err := q.pool.QueryRow(ctx, `
		SELECT vcf_uri FROM chunk_result
		 WHERE chunk_id = (SELECT result_chunk_id FROM job_state WHERE id=$1)`, jobID).Scan(&uri)
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

// InputRef returns where a job's submitted input is stored, and whether it is
// stored at all rather than carried inline.
func (q *Queue) InputRef(ctx context.Context, jobID string) (string, bool, error) {
	var uri *string
	err := q.pool.QueryRow(ctx, `
		SELECT uri FROM chunk_input
		 WHERE chunk_id = (SELECT input_chunk_id FROM job WHERE id=$1)`, jobID).Scan(&uri)
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

// DropInput deletes a chunk's input row, and reports the storage URI it
// referred to so the caller can delete the object too.
//
// Two steps rather than one, and deliberately not transactional: an object
// left behind after the row is gone is scrap that the storage sweep collects,
// while a row pointing at an object that has been deleted is a chunk that
// looks runnable and is not. Losing the pointer last is the safe order.
func (q *Queue) DropInput(ctx context.Context, id string) (string, error) {
	var uri *string
	err := q.pool.QueryRow(ctx,
		`DELETE FROM chunk_input WHERE chunk_id=$1 RETURNING uri`, id).Scan(&uri)
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

// firstChunk reports whether this chunk is where a job's results table starts.
//
// The sole chunk of an ordinary submission, or piece 0 of a split one. A split's
// own chunk and its collect are neither: the first produces no variants and the
// second's output is the join, which is every piece over again.
func firstChunk(c Chunk) bool {
	return c.ChunkIndex == nil || *c.ChunkIndex == 0
}

type rowScanner interface{ Scan(dest ...any) error }

func scanChunk(row rowScanner) (Chunk, error) {
	var j Chunk
	if err := row.Scan(&j.ID, &j.Kind, &j.Snapshot, &j.Selection, &j.Status,
		&j.Error, &j.NVariants, &j.ClientIP, &j.Session, &j.UserID, &j.Label, &j.Weight,
		&j.Origin, &j.MaxVariants, &j.JobID, &j.ChunkIndex, &j.AwaitsPieces,
		&j.CompletesJob, &j.CreatedAt, &j.StartedAt, &j.FinishedAt); err != nil {
		return Chunk{}, err
	}
	return j, nil
}

// ChunkFilter narrows a List query. Empty fields are not constrained.
type ChunkFilter struct {
	Status   string   // queued|running|done|error
	Session  string   // scope to one submitter's session id
	UserID   string   // scope to one account
	ClientIP string   // scope to one client IP
	Kinds    []string // restrict to these chunk kinds
}

// List returns chunks newest-first matching the filter, with limit/offset
// paging.
func (q *Queue) List(ctx context.Context, f ChunkFilter, limit, offset int) ([]Chunk, error) {
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
	query := `SELECT ` + chunkCols + ` FROM chunk`
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
	var out []Chunk
	for rows.Next() {
		j, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeleteOlderThan removes terminal jobs (finished_at set) whose finished_at is
// before cutoff, along with their chunks and every blob those own. Queued and
// running jobs are never touched. Returns the number of jobs deleted.
//
// Jobs rather than chunks, since 0037: a chunk belongs to a job and goes when
// it does. Ageing chunks out on their own would leave a split job whose pieces
// had been collected while the job still claimed to have twenty-six of them.
//
// The chunks go with the job, and the blobs with the chunks, via ON DELETE
// CASCADE — so unlike the SQLite version this is one statement rather than
// several inside a transaction.
func (q *Queue) DeleteOlderThan(ctx context.Context, cutoff int64) (int64, error) {
	// The objects these jobs own — inputs and built results alike — read before
	// their rows go.
	//
	// chunk_input cascades, so deleting the row destroys the only record of
	// where the object was — and an object nothing points at is invisible to
	// everything short of listing the whole bucket. Without this, every VCF job
	// that ages out leaves its input behind for good, and storage grows without
	// limit while the table it was tracked in shrinks on schedule.
	var owned []string
	if rows, err := q.pool.Query(ctx, `
		SELECT i.uri FROM chunk_input i
		  JOIN chunk c ON c.id = i.chunk_id
		  JOIN job_state j ON j.id = c.job_id
		 WHERE i.uri IS NOT NULL
		   AND j.finished_at IS NOT NULL AND j.finished_at < $1
		UNION ALL
		SELECT r.vcf_uri FROM chunk_result r
		  JOIN chunk c ON c.id = r.chunk_id
		  JOIN job_state j ON j.id = c.job_id
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
		// objects, and a leaked object is recoverable by a listing while a
		// table that never shrinks is not.
		log.Printf("queue: could not list the objects of expiring jobs: %v", err)
	}

	// Through the view: a job's finish time is its chunks' — see job_state — so
	// there is no column here to compare against.
	tag, err := q.pool.Exec(ctx, `
		DELETE FROM job WHERE id IN (
		  SELECT id FROM job_state
		   WHERE finished_at IS NOT NULL AND finished_at < $1)`, cutoff)
	if err != nil {
		return 0, err
	}
	// After the rows, deliberately. This order can leak an object when the
	// disposal fails; the other order can leave a job that still looks
	// complete with its input already gone, which is a wrong answer rather
	// than waste.
	if len(owned) > 0 && q.disposeObjects != nil {
		q.disposeObjects(ctx, owned)
	}

	// The fair-share timestamps age out with the chunks. Safe because the
	// ordering reads GREATEST(created_at, last_finished_at): once the
	// timestamp is older than any queued chunk could be, it never wins that
	// comparison, so an absent row and a sufficiently old one give the same
	// answer. Without this the table grows a row per anonymous address
	// forever.
	//
	// Best effort — this is housekeeping, and failing the sweep over it would
	// leave the chunks themselves uncollected.
	if _, err := q.pool.Exec(ctx,
		`DELETE FROM queue_caller WHERE last_finished_at < $1`, cutoff); err != nil {
		log.Printf("queue: prune fair-share timestamps: %v", err)
	}
	return tag.RowsAffected(), nil
}

// StartSweeper launches a goroutine that deletes terminal chunks older than
// ttl, sweeping once immediately and then every interval until ctx is
// cancelled. A ttl <= 0 disables GC (the goroutine is not started).
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
					log.Printf("queue: chunk GC: %v", err)
				}
			} else if n > 0 {
				log.Printf("queue: chunk GC removed %d chunk(s) older than %s", n, ttl)
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

// StartWorkers launches n worker goroutines that claim and process queued
// chunks until ctx is cancelled. Call Wait to block for their shutdown.
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
	// Fallback poll: covers a missed NOTIFY and the multi-replica case where a
	// chunk was enqueued by another process while this one held no listener.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		// Drain all currently-claimable chunks before sleeping.
		for {
			if ctx.Err() != nil {
				return
			}
			chunk, input, ok, err := q.claimNext(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("queue: claim chunk: %v", err)
				break
			}
			if !ok {
				break
			}
			q.process(ctx, chunk, input, runner)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.queued:
		case <-ticker.C:
		}
	}
}

// claimQuery claims the next chunk in a single statement.
//
// Fairness has two terms, covering two different ways one caller can crowd out
// another:
//
//   - fewest chunks *running* — stops one caller holding every slot at once.
//   - longest since they were *served* — stops one caller taking every slot in
//     sequence. A chunk's position is GREATEST(created_at, last_finished_at),
//     so finishing a chunk pushes the rest of that caller's queue behind
//     everyone who has been waiting.
//
// The second exists because the first is blind at one slot. By the time a
// worker claims, the chunk it just finished is already marked done, so with
// slots=1 every caller has zero running, the term is a constant, and the
// ordering collapses to plain FIFO — a caller who queued 400 chunks before
// anyone else arrived took all 400 in a row. More generally a
// concurrency-based signal can only separate callers up to the number of
// slots, and has nothing to say when there is one.
//
// Both are kept because each is blind where the other sees: last_finished_at
// only advances on completion, so a caller with a six-hour chunk in flight
// looks idle by that measure, and only the running count stops them taking
// more slots.
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
// tie and that burst drains FIFO. Deliberate: chunks completing that fast mean
// the queue is not contended, so there is nothing for fairness to protect, and
// the next tick resolves it. Finer resolution would buy ordering nobody is
// waiting on, at the cost of widening three columns and everything that reads
// them.
//
// FOR UPDATE OF j is required rather than a bare FOR UPDATE: Postgres refuses
// to lock the nullable side of an outer join, and r is a LEFT JOIN subquery.
// Locking only j is both legal and what we want. SKIP LOCKED is what lets N
// workers claim N distinct chunks concurrently instead of serializing on the
// same head-of-queue row.
//
// An idle pool takes the next chunk whatever it weighs. Without that, a chunk
// heavier than the entire budget is unclaimable rather than merely exclusive:
// on a one-slot pool 0+2 <= 1 is false, so a weight-2 download waits behind
// nothing at all, forever and silently. VHW_WORKERS=1 is an ordinary
// deployment and slots follow workers, so that is a default configuration, not
// a corner. Weight is there to stop chunks overlapping, not to make them
// unrunnable — one too big for the pool should run alone, which is what an
// empty pool already guarantees.
const claimQuery = `
UPDATE chunk SET status = $1, started_at = $2, claimed_by = $6, lease_until = $7,
               attempts = chunk.attempts + 1
WHERE id = (
  SELECT j.id
  FROM chunk j
  LEFT JOIN (
    SELECT COALESCE(NULLIF(user_id,''), client_ip) AS who, COUNT(*) AS c
      FROM chunk WHERE status = $1
     GROUP BY COALESCE(NULLIF(user_id,''), client_ip)
  ) r ON r.who = COALESCE(NULLIF(j.user_id,''), j.client_ip)
  LEFT JOIN queue_caller f
    ON f.who = COALESCE(NULLIF(j.user_id,''), j.client_ip)
  CROSS JOIN (
    SELECT COALESCE(SUM(weight),0) AS used FROM chunk WHERE status = $1
  ) p
  WHERE j.status = $3
    AND COALESCE(r.c, 0) < (CASE WHEN j.max_concurrent > 0 THEN j.max_concurrent ELSE $4 END)
    AND (p.used = 0 OR p.used + j.weight <= $5)
    -- A collect waits for the pieces it joins. It is queued with them rather
    -- than by whichever finishes last, so the work a job still owes is visible
    -- as rows; this is what keeps it out of a worker's hands until there is
    -- something to join. Exactly one worker claims it for the same reason
    -- exactly one worker claims anything, so no counter decides who starts it.
    AND (NOT j.awaits_pieces OR NOT EXISTS (
          SELECT 1 FROM chunk s
           WHERE s.job_id = j.job_id AND s.chunk_index IS NOT NULL
             AND s.status <> $8))
  ORDER BY COALESCE(r.c, 0) ASC,
           GREATEST(j.created_at, COALESCE(f.last_finished_at, 0)) ASC,
           j.created_at ASC, j.id ASC
  FOR UPDATE OF j SKIP LOCKED
  LIMIT 1
)
RETURNING ` + chunkCols

// claimNext atomically claims the next queued chunk, marking it running.
// ok=false when there is nothing claimable.
func (q *Queue) claimNext(ctx context.Context) (Chunk, []byte, bool, error) {
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
		return Chunk{}, nil, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "varhub-chunk-claim"); err != nil {
		return Chunk{}, nil, false, err
	}

	row := tx.QueryRow(ctx, claimQuery, StatusRunning, q.nowFn(), StatusQueued, maxPerIP, slots,
		q.workerID, q.nowFn()+int64(q.leaseTTL.Seconds()), StatusDone)
	chunk, err := scanChunk(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Chunk{}, nil, false, nil
	}
	if err != nil {
		return Chunk{}, nil, false, err
	}
	// Open this attempt's history row in the claim transaction, so a claimed
	// chunk always has one. Written apart, a crash in between would leave an
	// attempt that ran and was never recorded — and the rows this table exists
	// for are exactly the ones whose worker did not survive to write anything
	// later.
	//
	// The attempt number is read back from the row the UPDATE just incremented
	// rather than counted here, which is what keeps it equal to chunk.attempts
	// instead of merely close to it.
	if _, err := tx.Exec(ctx, `
		INSERT INTO chunk_attempt (chunk_id, attempt, worker, started_at)
		SELECT id, attempts, $2, $3 FROM chunk WHERE id = $1
		ON CONFLICT (chunk_id, attempt) DO NOTHING`,
		chunk.ID, q.workerID, q.nowFn()); err != nil {
		return Chunk{}, nil, false, err
	}
	// Read the input inside the same transaction, so a claim and its input are
	// one atomic step: committing the claim and then failing to read it would
	// leave a chunk marked running that no worker is running.
	//
	// A stored input yields a URI and no bytes. The claim only needs to know
	// it exists — staging it is the runner's chunk, and doing it here would
	// hold the claim transaction open for the length of a download.
	var body []byte
	var uri *string
	if err := tx.QueryRow(ctx,
		`SELECT body, uri FROM chunk_input WHERE chunk_id=$1`, chunk.ID).Scan(&body, &uri); err != nil {
		return Chunk{}, nil, false, err
	}
	if uri != nil {
		chunk.InputURI = *uri
	}
	if err := tx.Commit(ctx); err != nil {
		return Chunk{}, nil, false, err
	}
	return chunk, body, true, nil
}

// process runs the chunk's runner and records its outcome. runningChunk is a
// chunk executing in this process, and the handle to stop it.
type runningChunk struct {
	cancel    context.CancelFunc
	cancelled bool // set when a cancel was requested, so the outcome says so
}

func (q *Queue) process(ctx context.Context, chunk Chunk, input []byte, runner Runner) {
	start := time.Now()
	log.Printf("queue: chunk %s running (kind=%s, ip=%s)", chunk.ID, chunk.Kind, chunk.ClientIP)

	// A context per chunk, so cancelling one does not touch the others this
	// worker has run or will run.
	runCtx, cancel := context.WithCancel(ctx)
	rj := &runningChunk{cancel: cancel}
	q.mu.Lock()
	q.running[chunk.ID] = rj
	q.mu.Unlock()
	defer func() {
		cancel()
		q.mu.Lock()
		delete(q.running, chunk.ID)
		q.mu.Unlock()
	}()

	out, err := runner(runCtx, chunk, input)

	q.mu.Lock()
	cancelled := rj.cancelled
	q.mu.Unlock()
	if cancelled {
		// Whatever the subprocess reported on the way down, the reason it went
		// down is known and is not a failure.
		log.Printf("queue: chunk %s cancelled after %s",
			chunk.ID, time.Since(start).Round(time.Millisecond))
		q.finish(ctx, chunk, StatusCancelled, "cancelled", Outcome{})
		return
	}
	if err != nil {
		log.Printf("queue: chunk %s failed after %s: %v",
			chunk.ID, time.Since(start).Round(time.Millisecond), err)
		q.finish(ctx, chunk, StatusError, err.Error(), Outcome{})
		return
	}
	q.finish(ctx, chunk, StatusDone, "", out)
	log.Printf("queue: chunk %s done (%d variant(s) in %s)",
		chunk.ID, out.N, time.Since(start).Round(time.Millisecond))
}

// finish records a chunk's terminal state and its result (if any), then
// notifies anyone blocked in WaitFor. Status, result and notification commit
// together, so a waiter woken by the NOTIFY always finds the row already
// terminal.
func (q *Queue) finish(ctx context.Context, chunk Chunk, status, errMsg string, out Outcome) {
	id := chunk.ID
	// Use a background context for the write: the chunk itself may have been
	// cancelled, but its outcome still has to be persisted.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	tx, err := q.pool.Begin(wctx)
	if err != nil {
		log.Printf("queue: finish chunk %s: %v", id, err)
		return
	}
	defer tx.Rollback(wctx) //nolint:errcheck // no-op after Commit

	// Where the answer is, not the answer. The variants themselves went into
	// storage as a VCF and into chunk_variant as rows; this row holds the
	// pointer to the file.
	if out.VCFURI != "" {
		if _, err := tx.Exec(wctx,
			`INSERT INTO chunk_result (chunk_id,vcf_uri) VALUES ($1,$2)
			 ON CONFLICT (chunk_id) DO UPDATE SET vcf_uri = excluded.vcf_uri`,
			id, out.VCFURI); err != nil {
			log.Printf("queue: store chunk %s result: %v", id, err)
			return
		}
	}
	// Rows for querying. Same transaction as the status change, so a chunk is
	// never observably "done" with results that are not yet queryable.
	//
	// Skipped for a download: its result is a file manifest, not variants, and
	// forcing it through the variant projection would fail on a shape that was
	// never meant to fit.
	//
	// Taken only from a job's first chunk. These rows are a window onto the
	// start of the result, so later pieces have nothing to add to it — and
	// reading only the first is what lets the cap be applied by one worker
	// looking at its own chunk, with no counter shared between the twenty-six
	// that may be finishing at the same moment.
	if out.Variants && out.Result != nil && firstChunk(chunk) {
		if err := insertVariants(wctx, tx, id, out.Result, out.MaxRows); err != nil {
			log.Printf("queue: store chunk %s variants: %v", id, err)
			return
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
		`UPDATE chunk SET status=$1, error=$2, n_variants=$3, finished_at=$4, columns=$5 WHERE id=$6`,
		status, errArg, out.N, q.nowFn(), colArg, id); err != nil {
		log.Printf("queue: finish chunk %s: %v", id, err)
		return
	}
	// Close this chunk's open attempt with the same outcome, in the same
	// transaction. Identified by "the one still open" rather than by number,
	// because finish() is reached from paths that never saw the claim — and
	// there is at most one, since claiming opens exactly one and every
	// terminal path closes it.
	//
	// No row is a normal case, not an error: a queued chunk cancelled before
	// any worker took it never had an attempt.
	if _, err := tx.Exec(wctx, `
		UPDATE chunk_attempt SET ended_at=$2, outcome=$3, error=$4
		 WHERE chunk_id=$1 AND outcome IS NULL`,
		id, q.nowFn(), status, errArg); err != nil {
		log.Printf("queue: close attempt for chunk %s: %v", id, err)
		return
	}
	// In the same transaction as the finish, so the scheduler can never see a
	// chunk completed without the charge for it having landed. Apart, a crash
	// between the two would leave that caller's next chunk holding a position
	// it has already spent.
	if err := chargeCaller(wctx, tx, id, q.nowFn()); err != nil {
		log.Printf("queue: charge caller for chunk %s: %v", id, err)
		return
	}
	if err := tx.Commit(wctx); err != nil {
		log.Printf("queue: commit chunk %s: %v", id, err)
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

// chargeCaller records that a chunk's caller has just been served, which
// pushes the rest of their queue behind everyone who has been waiting.
//
// The identity is derived from the chunk row in SQL rather than from a Go
// field, so it is the same expression the claim query orders by. Written
// twice, the two could disagree — and a chunk charged to one identity while
// ordered under another is a scheduler that quietly stops being fair.
//
// GREATEST, not assignment: a cancel and a completion can land out of order, and
// moving the timestamp backwards would hand back a turn that was already taken.
//
// A chunk with neither an account nor an address has no caller to be fair
// between, so the WHERE drops it rather than filing everything anonymous under
// one key.
func chargeCaller(ctx context.Context, tx pgx.Tx, id string, now int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO queue_caller (who, last_finished_at)
		SELECT COALESCE(NULLIF(user_id,''), client_ip), $2
		  FROM chunk
		 WHERE id = $1 AND COALESCE(NULLIF(user_id,''), client_ip) <> ''
		ON CONFLICT (who) DO UPDATE
		   SET last_finished_at = GREATEST(queue_caller.last_finished_at, excluded.last_finished_at)`,
		id, now)
	return err
}

// WorkerID identifies this process in the claims it holds, for logs that need
// to say which worker did something.
func (q *Queue) WorkerID() string { return q.workerID }

// AppendLog adds to a chunk's recorded output without reading it back first.
//
// Concatenated in SQL rather than read-modify-written, so two writers — the
// worker streaming its run, and a peer noting that the chunk was abandoned —
// cannot lose each other's lines.
func (q *Queue) AppendLog(ctx context.Context, id, s string) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO chunk_log (chunk_id, output) VALUES ($1,$2)
		ON CONFLICT (chunk_id) DO UPDATE SET output = chunk_log.output || EXCLUDED.output`,
		id, s)
	return err
}

// RunningIDs returns the ids of chunks currently claimed and running.
//
// The queue is authoritative about what is in flight, which is what lets a
// worker reclaim another's leftovers without guessing: scratch named after a
// chunk not in this set belongs to nobody.
func (q *Queue) RunningIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := q.pool.Query(ctx, `SELECT id FROM chunk WHERE status=$1`, StatusRunning)
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
