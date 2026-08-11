package anncache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Like the catalog and queue tests, these need a real Postgres; see
// internal/queue for the container invocation. Each test gets its own schema.

// allMigrations are every migration, discovered rather than listed, so a new one
// is exercised by the existing tests the moment it lands.
func allMigrations(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found; the glob or the layout has moved")
	}
	sort.Strings(files)
	return files
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping anncache tests")
	}
	ctx := context.Background()

	var ddl strings.Builder
	for _, f := range allMigrations(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ddl.Write(b)
		ddl.WriteString("\n")
	}

	schema := fmt.Sprintf("a_%d", time.Now().UnixNano())
	setup, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := setup.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		setup.Close()
		t.Fatalf("create schema: %v", err)
	}
	if _, err := setup.Exec(ctx, `SET search_path TO `+schema+`; `+ddl.String()); err != nil {
		setup.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	setup.Close()

	pool, err := pgxpool.New(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		drop, err := pgxpool.New(context.Background(), dsn)
		if err == nil {
			_, _ = drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			drop.Close()
		}
		pool.Close()
	})
	return New(pool)
}

const assembly = "GRCh38"

func loc(chrom string, pos int64, ref, alt string) Locus {
	return Locus{Chrom: chrom, Pos: pos, Ref: ref, Alt: alt}
}

// at fixes the clock so entries can be aged without sleeping.
func at(s *Store, sec int64) { s.nowFn = func() int64 { return sec } }

func TestPutAndLookupRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	err := s.Put(ctx, assembly, []Unit{{
		Locus:  l,
		Source: "gnomad:4.1",
		Entries: Hit{
			"af":            {Num: 0.0123, IsNum: true},
			"filter_status": {Str: "PASS"},
		},
	}})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	hits, err := s.Lookup(ctx, assembly, []Locus{l}, []string{"gnomad:4.1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	hit, ok := hits.Get(l.Key(), "gnomad:4.1")
	if !ok {
		t.Fatal("Lookup: unit not found after Put")
	}
	if got := hit["af"]; !got.IsNum || got.Num != 0.0123 {
		t.Errorf("af = %+v, want the number 0.0123", got)
	}
	// A numeric value that came back as text would sort as a string in the UI,
	// which reads as bad data rather than as a cache.
	if got := hit["filter_status"]; got.IsNum || got.Str != "PASS" {
		t.Errorf("filter_status = %+v, want the string PASS", got)
	}
}

// An empty unit is the whole point of caching a negative answer: without it, a
// common variant no source annotates is recomputed on every job forever.
func TestEmptyUnitIsAHitNotAMiss(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cached := loc("chr1", 100, "A", "T")
	never := loc("chr1", 200, "G", "C")

	if err := s.Put(ctx, assembly, []Unit{{
		Locus: cached, Source: "clinvar:2026-01", Entries: Hit{},
	}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hits, err := s.Lookup(ctx, assembly, []Locus{cached, never}, []string{"clinvar:2026-01"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	hit, ok := hits.Get(cached.Key(), "clinvar:2026-01")
	if !ok {
		t.Fatal("the empty unit read as a miss; it will be recomputed forever")
	}
	if len(hit) != 0 {
		t.Errorf("empty unit came back with %d entries", len(hit))
	}
	if _, ok := hits.Get(never.Key(), "clinvar:2026-01"); ok {
		t.Error("a locus never written read as a hit")
	}
}

// Scoping the lookup to the job's own sources is what makes entitlement fall out
// of the design: a source the requester was never granted is not in the query.
func TestLookupIsScopedToTheSourcesAsked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	if err := s.Put(ctx, assembly, []Unit{
		{Locus: l, Source: "public:1", Entries: Hit{"a": {Str: "x"}}},
		{Locus: l, Source: "private:1", Entries: Hit{"b": {Str: "secret"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hits, err := s.Lookup(ctx, assembly, []Locus{l}, []string{"public:1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := hits.Get(l.Key(), "private:1"); ok {
		t.Error("a source outside the job's selection came back from the cache")
	}
	if _, ok := hits.Get(l.Key(), "public:1"); !ok {
		t.Error("the requested source did not come back")
	}
}

// Two assemblies are two different variants at the same coordinates. Serving one
// as the other is the failure mode that never announces itself.
func TestAssemblySeparatesEntries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	if err := s.Put(ctx, "GRCh37", []Unit{
		{Locus: l, Source: "gnomad:4.1", Entries: Hit{"af": {Num: 0.5, IsNum: true}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	hits, err := s.Lookup(ctx, "GRCh38", []Locus{l}, []string{"gnomad:4.1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := hits.Get(l.Key(), "gnomad:4.1"); ok {
		t.Error("a GRCh37 entry was served to a GRCh38 lookup")
	}
}

// A re-run's output is the source's whole answer for that variant, so a field it
// has stopped emitting must not survive beside the ones it still does.
func TestPutReplacesAUnitRatherThanMerging(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	if err := s.Put(ctx, assembly, []Unit{
		{Locus: l, Source: "src:1", Entries: Hit{"gone": {Str: "old"}, "kept": {Str: "old"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(ctx, assembly, []Unit{
		{Locus: l, Source: "src:1", Entries: Hit{"kept": {Str: "new"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hit, _ := mustHit(t, s, l, "src:1")
	if _, ok := hit["gone"]; ok {
		t.Error("a field the source no longer emits survived the rewrite")
	}
	if hit["kept"].Str != "new" {
		t.Errorf("kept = %q, want new", hit["kept"].Str)
	}
}

func TestSweepByAgeRemovesWholeUnits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := loc("chr1", 100, "A", "T")
	fresh := loc("chr1", 200, "G", "C")

	const day = 24 * 60 * 60
	at(s, 1_000_000*day)
	if err := s.Put(ctx, assembly, []Unit{
		{Locus: old, Source: "src:1", Entries: Hit{"a": {Str: "x"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	at(s, 1_000_010*day)
	if err := s.Put(ctx, assembly, []Unit{
		{Locus: fresh, Source: "src:1", Entries: Hit{"a": {Str: "y"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := s.Sweep(ctx, 0, 5*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ByAge != 1 {
		t.Errorf("ByAge = %d, want 1", res.ByAge)
	}

	hits, err := s.Lookup(ctx, assembly, []Locus{old, fresh}, []string{"src:1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := hits.Get(old.Key(), "src:1"); ok {
		t.Error("the aged-out unit is still cached")
	}
	if _, ok := hits.Get(fresh.Key(), "src:1"); !ok {
		t.Error("the sweep took a unit that was still within maxAge")
	}
	// The values must go with their parent. A unit whose entries outlived it
	// would be an orphan nothing can read or evict.
	var orphans int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM cache_entry e
		 WHERE NOT EXISTS (SELECT 1 FROM cache_variant_source vs WHERE vs.id = e.vs_id)
	`).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d entries outlived their unit", orphans)
	}
}

// A lookup is also a use: an entry read a minute ago must not be the one the age
// sweep takes.
func TestLookupDefersTheAgeSweep(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	const day = 24 * 60 * 60
	at(s, 1_000_000*day)
	if err := s.Put(ctx, assembly, []Unit{
		{Locus: l, Source: "src:1", Entries: Hit{"a": {Str: "x"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	at(s, 1_000_010*day)
	if _, err := s.Lookup(ctx, assembly, []Locus{l}, []string{"src:1"}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	res, err := s.Sweep(ctx, 0, 5*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ByAge != 0 {
		t.Fatalf("ByAge = %d; the read did not count as a use", res.ByAge)
	}
}

func TestSweepByCountTakesTheLeastRecentlyUsed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const hour = 3600
	base := int64(1_000_000 * hour)
	loci := []Locus{
		loc("chr1", 100, "A", "T"),
		loc("chr1", 200, "A", "T"),
		loc("chr1", 300, "A", "T"),
		loc("chr1", 400, "A", "T"),
	}
	for i, l := range loci {
		at(s, base+int64(i)*hour)
		if err := s.Put(ctx, assembly, []Unit{
			{Locus: l, Source: "src:1", Entries: Hit{"a": {Str: "x"}}},
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// The cap is checked against the planner's estimate, which autovacuum has not
	// caught up on for a table written a moment ago.
	if err := s.Analyze(ctx); err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	res, err := s.Sweep(ctx, 2, 0, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ByCount != 2 {
		t.Fatalf("ByCount = %d, want 2 (before=%d)", res.ByCount, res.Before)
	}

	hits, err := s.Lookup(ctx, assembly, loci, []string{"src:1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	for i, l := range loci {
		_, ok := hits.Get(l.Key(), "src:1")
		if want := i >= 2; ok != want {
			t.Errorf("locus %d cached = %v, want %v; the sweep did not take the oldest first", i, ok, want)
		}
	}
}

func TestSweepDeclinesWhenAnotherHoldsTheLock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	held, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()
	var got bool
	if err := held.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1, hashtext(current_schema()))`,
		int32(sweepLockClass)).Scan(&got); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if !got {
		t.Fatal("could not take the lock to hold it")
	}

	res, err := s.Sweep(ctx, 1, 0, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !res.Skipped {
		t.Error("a second sweeper ran while the lock was held")
	}
}

// A sweep with nothing to enforce must not be a sweep that removes everything.
func TestSweepWithNoPolicyDoesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	if err := s.Put(ctx, assembly, []Unit{
		{Locus: l, Source: "src:1", Entries: Hit{"a": {Str: "x"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	res, err := s.Sweep(ctx, 0, 0, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Removed() != 0 {
		t.Errorf("Removed = %d, want 0", res.Removed())
	}
	mustHit(t, s, l, "src:1")
}

func TestPurgeSourceLeavesTheOthers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	if err := s.Put(ctx, assembly, []Unit{
		{Locus: l, Source: "edited:1", Entries: Hit{"a": {Str: "x"}}},
		{Locus: l, Source: "other:1", Entries: Hit{"b": {Str: "y"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PurgeSource(ctx, "edited:1"); err != nil {
		t.Fatalf("PurgeSource: %v", err)
	}

	hits, err := s.Lookup(ctx, assembly, []Locus{l}, []string{"edited:1", "other:1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := hits.Get(l.Key(), "edited:1"); ok {
		t.Error("the edited source's entries survived the purge")
	}
	if _, ok := hits.Get(l.Key(), "other:1"); !ok {
		t.Error("the purge took a source it was not asked about")
	}
}

func TestClearEmptiesTheCache(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	l := loc("chr1", 100, "A", "T")

	if err := s.Put(ctx, assembly, []Unit{
		{Locus: l, Source: "src:1", Entries: Hit{"a": {Str: "x"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	hits, err := s.Lookup(ctx, assembly, []Locus{l}, []string{"src:1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Clear left %d loci cached", len(hits))
	}
}

func TestValueOfRejectsWhatItCannotRepresent(t *testing.T) {
	// A null is the absence of a value, already represented by the entry not
	// being there; a bool or an array is a shape this cache does not model, and
	// guessing is how a cached value comes back different from a computed one.
	for _, v := range []any{nil, true, []any{"a"}, map[string]any{}} {
		if _, ok := ValueOf(v); ok {
			t.Errorf("ValueOf(%#v) accepted a value it cannot round-trip", v)
		}
	}
	if got, ok := ValueOf(1.5); !ok || got.Any() != any(1.5) {
		t.Errorf("ValueOf(1.5) = %+v, %v", got, ok)
	}
	if got, ok := ValueOf("x"); !ok || got.Any() != any("x") {
		t.Errorf("ValueOf(\"x\") = %+v, %v", got, ok)
	}
}

func mustHit(t *testing.T, s *Store, l Locus, source string) (Hit, bool) {
	t.Helper()
	hits, err := s.Lookup(context.Background(), assembly, []Locus{l}, []string{source})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	hit, ok := hits.Get(l.Key(), source)
	if !ok {
		t.Fatalf("no cached unit for %s / %s", l.Key(), source)
	}
	return hit, ok
}
