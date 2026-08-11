package anncache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// sweepLockClass identifies this application's advisory locks.
//
// An advisory lock rather than a table lock, deliberately. Locking the cache
// would stall every annotation for the length of the sweep, which on a large
// cache is an outage on the hot path in aid of housekeeping. This only stops two
// sweepers running at once, which is the actual requirement — and Postgres
// releases it if the holder's session dies, so a killed worker leaves nothing to
// clear by hand.
const sweepLockClass = 0x7661_7268 // "varh"

// defaultBatch bounds one delete statement.
const defaultBatch = 10_000

// SweepResult reports what a sweep did, in (variant, source) units.
type SweepResult struct {
	Before  int64 // estimated units before the sweep
	ByAge   int64 // units removed for being unused too long
	ByCount int64 // units removed to get back under the cap
	Skipped bool  // another sweeper held the lock
}

// Removed is the total this sweep took out.
func (r SweepResult) Removed() int64 { return r.ByAge + r.ByCount }

// Sweep trims the cache: first whatever has gone unused longer than maxAge, then
// the least recently used until at most maxEntries units remain. A zero maxAge or
// maxEntries means that policy is unbounded, and a sweep with both unbounded does
// nothing.
//
// Age before count, because age is the policy an administrator states in terms of
// correctness ("nothing older than 90 days") and count is the one stated in terms
// of resources. Running age first means the cap is applied to entries that are
// all still within the age the deployment considers acceptable, and on a cache
// under its cap the age pass still runs.
//
// Counted in units rather than values: entries per unit are bounded by how many
// fields a source declares, so the parent count is a stable proxy for total size
// and needs no denormalized counter kept true by every writer.
func (s *Store) Sweep(ctx context.Context, maxEntries int64, maxAge time.Duration, batch int64) (SweepResult, error) {
	if maxEntries < 0 {
		return SweepResult{}, fmt.Errorf("anncache: maxEntries must not be negative, got %d", maxEntries)
	}
	if maxAge < 0 {
		return SweepResult{}, fmt.Errorf("anncache: maxAge must not be negative, got %s", maxAge)
	}
	if maxEntries == 0 && maxAge == 0 {
		return SweepResult{}, nil
	}
	if batch <= 0 {
		batch = defaultBatch
	}

	// One sweeper at a time. Losing the race is not a failure — it means somebody
	// else is already doing this.
	//
	// Keyed by schema as well as application: a deployment's cache lives in one
	// schema, and two of them in one database (as the tests do) are two separate
	// caches that should not queue behind each other.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return SweepResult{}, fmt.Errorf("anncache: sweep: %w", err)
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1, hashtext(current_schema()))`,
		int32(sweepLockClass)).Scan(&got); err != nil {
		return SweepResult{}, fmt.Errorf("anncache: sweep lock: %w", err)
	}
	if !got {
		return SweepResult{Skipped: true}, nil
	}
	defer conn.Exec(ctx, //nolint:errcheck // released with the session in any case
		`SELECT pg_advisory_unlock($1, hashtext(current_schema()))`, int32(sweepLockClass))

	before, err := s.approxCount(ctx)
	if err != nil {
		return SweepResult{}, err
	}
	res := SweepResult{Before: before}

	if maxAge > 0 {
		cutoff := hourOf(s.nowFn()) - int64(maxAge/time.Second)
		n, err := s.deleteBatched(ctx, batch, 0, `
			DELETE FROM cache_variant_source
			 WHERE id IN (
			   SELECT id FROM cache_variant_source
			    WHERE last_used < $2
			    LIMIT $1
			 )`, cutoff)
		res.ByAge = n
		if err != nil {
			return res, err
		}
	}

	if maxEntries > 0 {
		// The age pass may have taken the cache under the cap already; re-estimating
		// after a large delete costs an ANALYZE we do not want on the sweep path, so
		// subtract what we know we removed instead.
		remaining := before - res.ByAge
		if remaining > maxEntries {
			n, err := s.deleteBatched(ctx, batch, remaining-maxEntries, `
				DELETE FROM cache_variant_source
				 WHERE id IN (
				   SELECT id FROM cache_variant_source
				    ORDER BY last_used ASC
				    LIMIT $1
				 )`)
			res.ByCount = n
			if err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// deleteBatched runs a delete repeatedly until it removes target rows, or until
// it removes nothing more if target is zero.
//
// Batched because one statement removing millions of rows holds its locks for the
// whole duration and writes a WAL record to match; a batch's locks are brief and
// only touch what is going. Each batch is its own transaction, so an interrupted
// sweep leaves the cache smaller rather than unchanged.
//
// The query takes the batch size as $1 and any extra arguments after it.
func (s *Store) deleteBatched(ctx context.Context, batch, target int64, query string, args ...any) (int64, error) {
	var done int64
	for {
		n := batch
		if target > 0 {
			if remaining := target - done; remaining <= 0 {
				return done, nil
			} else if remaining < n {
				n = remaining
			}
		}
		tag, err := s.pool.Exec(ctx, query, append([]any{n}, args...)...)
		if err != nil {
			return done, fmt.Errorf("anncache: sweep: %w", err)
		}
		removed := tag.RowsAffected()
		done += removed
		// Nothing left to take. For the count pass this means the estimate was
		// high, which is expected and not an error.
		if removed < n {
			return done, nil
		}
	}
}

// approxCount is the planner's row estimate for the unit table.
//
// Maintained by autovacuum and read from the catalog in constant time. It is
// wrong by a few percent between analyses, which for "is the cache over budget"
// is indistinguishable from right — and the alternative, count(*), is a
// sequential scan of tens of millions of rows every sweep to learn the same
// thing.
//
// A negative estimate means the table has never been analysed; treated as zero,
// so a brand-new cache is not evicted on the strength of a missing statistic.
func (s *Store) approxCount(ctx context.Context) (int64, error) {
	var est float64
	err := s.pool.QueryRow(ctx,
		`SELECT reltuples FROM pg_class WHERE oid = to_regclass('cache_variant_source')`).Scan(&est)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // not migrated here yet
	}
	if err != nil {
		return 0, fmt.Errorf("anncache: count: %w", err)
	}
	if est < 0 {
		return 0, nil
	}
	return int64(est), nil
}

// Analyze updates the planner statistics Sweep reads.
//
// Called before a sweep by anything that needs the estimate to reflect a large
// recent write — autovacuum gets there on its own schedule, which is the right
// default but can be an hour behind a bulk load.
func (s *Store) Analyze(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `ANALYZE cache_variant_source`)
	if err != nil {
		return fmt.Errorf("anncache: analyze: %w", err)
	}
	return nil
}
