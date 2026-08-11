// Package anncache is the shared annotation cache: what a pinned source has to
// say about a variant, kept in Postgres so every worker in a deployment benefits
// from what any one of them computed.
//
// It lives here rather than inside varhub because varhub runs a source's tool
// steps as bash. A cache DSN written into a job's home is readable by any code a
// registered manifest chooses to run, which turns "can register a source" into
// read and write on accounts, tokens and results. Caching a layer up removes the
// credential from that blast radius entirely.
//
// Caching at the value layer also subsumes varhub's own internal cache: a
// variant that hits is never sent to the CLI at all, so the job skips the process
// spawn, the home materialization, the source reads and the container startup —
// none of which a cache living inside varhub can avoid.
//
// # What may be cached
//
// Only values that are a pure function of the variant. Fields computed from a
// sample's FORMAT columns (dosage, VAF, strand bias) or from neighbouring
// variants are not the same answer for two callers asking about the same locus,
// and caching one would serve one sample's number as another's. That check
// belongs to the caller building the units; nothing here can detect it.
package anncache

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Locus is one variant, as the cache keys it. Assembly is carried separately
// because it is the same for every locus in a job.
type Locus struct {
	Chrom string
	Pos   int64
	Ref   string
	Alt   string
}

// Key is the locus's identity as a map key, matching varhub's own locus key so
// the two can be compared without a conversion.
//
// It is also the form varhub takes on the command line, which is not a
// coincidence worth relying on: Arg says so explicitly, so a change to either
// does not silently become a change to the other.
func (l Locus) Key() string {
	return l.Chrom + ":" + strconv.FormatInt(l.Pos, 10) + ":" + l.Ref + ":" + l.Alt
}

// Arg renders the locus as varhub expects it on the command line.
func (l Locus) Arg() string {
	return l.Chrom + ":" + strconv.FormatInt(l.Pos, 10) + ":" + l.Ref + ":" + l.Alt
}

// Value is one annotation's value, kept in the two shapes varhub emits: a JSON
// number or a JSON string.
//
// The distinction is preserved rather than flattened to text because it is
// visible in the result — a column declared numeric that comes back quoted sorts
// as a string in the UI, which looks like bad data rather than a cache.
type Value struct {
	Num   float64
	Str   string
	IsNum bool
}

// ValueOf converts a decoded JSON annotation value into a cacheable Value.
//
// Reports false for null, which is not a value but the absence of one: varhub
// emits a key for every selected annotation and nulls the ones with no match, so
// a null is already represented by the entry simply not being there. Also false
// for anything else — a bool or an array would be a shape this cache does not
// model, and guessing at it is how a cached value comes back different from a
// computed one.
func ValueOf(v any) (Value, bool) {
	switch t := v.(type) {
	case nil:
		return Value{}, false
	case float64:
		return Value{Num: t, IsNum: true}, true
	case string:
		return Value{Str: t}, true
	default:
		return Value{}, false
	}
}

// Any renders a Value back as the JSON value varhub would have emitted.
func (v Value) Any() any {
	if v.IsNum {
		return v.Num
	}
	return v.Str
}

// Hit is what one source had to say about one variant: annotation name (the
// manifest's, not the prefixed one) to value.
//
// An empty but non-nil Hit is the meaningful state "asked, nothing to say". A
// caller must distinguish it from a missing source in Hits — the first is a hit,
// the second is a miss.
type Hit map[string]Value

// Hits is a lookup's result: locus key to source ref to what that source said.
// A source absent for a locus was not cached for it.
type Hits map[string]map[string]Hit

// Get returns what one source said about one locus, and whether it was cached.
func (h Hits) Get(locus, source string) (Hit, bool) {
	bySource, ok := h[locus]
	if !ok {
		return nil, false
	}
	hit, ok := bySource[source]
	return hit, ok
}

// Unit is one (variant, source) pair's whole answer, as written.
//
// Entries may be empty, and writing it that way is the point: without the empty
// unit, a common variant that no source annotates is recomputed on every job
// forever.
type Unit struct {
	Locus   Locus
	Source  string
	Entries Hit
}

// Store is the cache on Postgres.
type Store struct {
	pool *pgxpool.Pool
	// nowFn is injectable so tests can age entries without sleeping.
	nowFn func() int64
}

// New wraps an existing pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, nowFn: func() int64 { return time.Now().Unix() }}
}

// Open connects to Postgres and returns a Store. The caller must Close it.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("anncache: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("anncache: ping: %w", err)
	}
	return New(pool), nil
}

// Close releases the pool. Only call it on a Store that owns one — a Store from
// New shares its caller's pool.
func (s *Store) Close() { s.pool.Close() }

// hourOf rounds a timestamp down to the hour.
//
// The LRU only needs to know roughly what has been used lately, and rounding
// turns "one write per read" into "one write per variant per hour" — on a table
// read by every annotation, that is the difference between a timestamp column
// and a vacuum problem.
func hourOf(sec int64) int64 { return sec - sec%3600 }

// Lookup returns what is cached for these loci from these sources.
//
// Scoped to the sources given, which is what makes entitlement fall out of the
// design rather than needing a check of its own: a caller passes the sources in
// this job's resolved selection, so a private source the requester was never
// granted is not in the query and cannot appear in the answer.
func (s *Store) Lookup(ctx context.Context, assembly string, loci []Locus, sources []string) (Hits, error) {
	out := Hits{}
	if len(loci) == 0 || len(sources) == 0 {
		return out, nil
	}
	chrom, pos, ref, alt := columns(loci)

	// LEFT JOIN, so a parent with no entries comes back as one row with a null
	// key. An inner join would drop exactly the units that mean "computed,
	// nothing to say", making them permanent misses.
	rows, err := s.pool.Query(ctx, `
		SELECT vs.chrom, vs.pos, vs.ref, vs.alt, vs.source, e.key, e.value_text, e.value_num
		  FROM cache_variant_source vs
		  JOIN unnest($2::text[], $3::bigint[], $4::text[], $5::text[])
		       AS want(chrom, pos, ref, alt)
		    ON want.chrom = vs.chrom AND want.pos = vs.pos
		   AND want.ref = vs.ref AND want.alt = vs.alt
		  LEFT JOIN cache_entry e ON e.vs_id = vs.id
		 WHERE vs.assembly = $1 AND vs.source = ANY($6)`,
		assembly, chrom, pos, ref, alt, sources)
	if err != nil {
		return nil, fmt.Errorf("anncache: lookup: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var l Locus
		var source string
		var key, text *string
		var num *float64
		if err := rows.Scan(&l.Chrom, &l.Pos, &l.Ref, &l.Alt, &source, &key, &text, &num); err != nil {
			return nil, fmt.Errorf("anncache: lookup: %w", err)
		}
		lk := l.Key()
		bySource, ok := out[lk]
		if !ok {
			bySource = map[string]Hit{}
			out[lk] = bySource
		}
		hit, ok := bySource[source]
		if !ok {
			hit = Hit{}
			bySource[source] = hit
		}
		if key != nil {
			hit[*key] = valueOf(text, num)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("anncache: lookup: %w", err)
	}
	if len(out) > 0 {
		s.touch(ctx, assembly, chrom, pos, ref, alt)
	}
	return out, nil
}

// touch marks these variants as recently used, at hour granularity.
//
// Best effort and deliberately not fatal: failing an annotation because an LRU
// timestamp could not be written would trade a correct answer for a bookkeeping
// detail. The cost of losing one is that an entry looks staler than it is.
func (s *Store) touch(ctx context.Context, assembly string, chrom []string, pos []int64, ref, alt []string) {
	now := hourOf(s.nowFn())
	_, _ = s.pool.Exec(ctx, `
		UPDATE cache_variant_source vs SET last_used = $6
		  FROM unnest($2::text[], $3::bigint[], $4::text[], $5::text[])
		       AS want(chrom, pos, ref, alt)
		 WHERE vs.assembly = $1 AND vs.last_used < $6
		   AND want.chrom = vs.chrom AND want.pos = vs.pos
		   AND want.ref = vs.ref AND want.alt = vs.alt`,
		assembly, chrom, pos, ref, alt, now)
}

// Put writes whole units, creating the (variant, source) parents the values hang
// off. A unit with no entries is written as a parent alone, which is how "asked,
// nothing to say" is recorded.
//
// One transaction, which is what makes this safe against a concurrent sweep:
// inserting an entry takes a foreign-key share lock on its parent, so a sweep
// deleting that parent either waits or is waited for. Neither side needs to know
// about the other, and no table lock is involved.
//
// A unit is replaced, not merged. It is the source's whole answer for that
// variant, so merging would leave a field the source has stopped emitting beside
// the ones it still does.
func (s *Store) Put(ctx context.Context, assembly string, units []Unit) error {
	if len(units) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("anncache: put: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	now := hourOf(s.nowFn())
	for _, u := range units {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO cache_variant_source (assembly,chrom,pos,ref,alt,source,last_used)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (assembly,chrom,pos,ref,alt,source)
			  DO UPDATE SET last_used = excluded.last_used
			RETURNING id`,
			assembly, u.Locus.Chrom, u.Locus.Pos, u.Locus.Ref, u.Locus.Alt,
			u.Source, now).Scan(&id); err != nil {
			return fmt.Errorf("anncache: put: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM cache_entry WHERE vs_id=$1`, id); err != nil {
			return fmt.Errorf("anncache: put: %w", err)
		}
		for k, v := range u.Entries {
			text, num := split(v)
			if _, err := tx.Exec(ctx,
				`INSERT INTO cache_entry (vs_id,key,value_text,value_num) VALUES ($1,$2,$3,$4)`,
				id, k, text, num); err != nil {
				return fmt.Errorf("anncache: put: %w", err)
			}
		}
	}
	return tx.Commit(ctx)
}

// Execer is the part of pgx a purge needs.
//
// Taken as an interface so a purge can join a transaction that belongs to
// somebody else. Invalidating a source's values is not an independent event: it
// is one half of "this source now emits something different", and the two have
// to land together or not at all.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PurgeSource removes everything cached for one source.
//
// For the case a version cannot express: editing a source's manifest changes
// which fields it emits and where they come from, while the dataset — and so the
// "name:version" the cache keys on — stays exactly as it was. Every entry under
// that key is now an answer to a question that is no longer being asked.
func (s *Store) PurgeSource(ctx context.Context, source string) error {
	return PurgeSource(ctx, s.pool, source)
}

// PurgeSource removes one source's values through the given connection or
// transaction. See Store.PurgeSource.
func PurgeSource(ctx context.Context, db Execer, source string) error {
	if _, err := db.Exec(ctx, `DELETE FROM cache_variant_source WHERE source=$1`, source); err != nil {
		return fmt.Errorf("anncache: purge %s: %w", source, err)
	}
	return nil
}

// Clear empties the cache, for an administrator starting over.
//
// TRUNCATE rather than DELETE: the point is to give the space back, and DELETE
// leaves that to a vacuum. CASCADE follows the foreign key to the values, the
// same one that makes eviction remove whole units rather than half of one.
func (s *Store) Clear(ctx context.Context) error {
	present, err := s.tablesPresent(ctx)
	if err != nil {
		return err
	}
	if len(present) == 0 {
		return nil // nothing migrated yet; nothing to clear is success
	}
	_, err = s.pool.Exec(ctx,
		`TRUNCATE TABLE `+strings.Join(present, ", ")+` RESTART IDENTITY CASCADE`)
	if err != nil {
		return fmt.Errorf("anncache: clear: %w", err)
	}
	return nil
}

// tablesPresent reports which cache tables exist, because TRUNCATE has no IF
// EXISTS and "the migration has not run here" is not an error worth showing an
// administrator.
func (s *Store) tablesPresent(ctx context.Context) ([]string, error) {
	var out []string
	for _, t := range []string{"cache_variant_source"} {
		var reg *string
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, t).Scan(&reg); err != nil {
			return nil, fmt.Errorf("anncache: %w", err)
		}
		if reg != nil {
			out = append(out, t)
		}
	}
	return out, nil
}

// --- helpers ---

func columns(loci []Locus) (chrom []string, pos []int64, ref, alt []string) {
	chrom = make([]string, len(loci))
	pos = make([]int64, len(loci))
	ref = make([]string, len(loci))
	alt = make([]string, len(loci))
	for i, l := range loci {
		chrom[i], pos[i], ref[i], alt[i] = l.Chrom, l.Pos, l.Ref, l.Alt
	}
	return
}

func split(v Value) (*string, *float64) {
	if v.IsNum {
		n := v.Num
		return nil, &n
	}
	t := v.Str
	return &t, nil
}

func valueOf(text *string, num *float64) Value {
	if num != nil {
		return Value{Num: *num, IsNum: true}
	}
	if text != nil {
		return Value{Str: *text}
	}
	return Value{}
}
