package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Like the queue tests, these need a real Postgres; see internal/queue for the
// container invocation. Each test gets its own schema.
var migrationFiles = []string{
	"../../migrations/0001_job_queue.sql",
	"../../migrations/0002_catalog.sql",
	"../../migrations/0004_registry.sql",
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping catalog tests")
	}
	ctx := context.Background()

	var ddl strings.Builder
	for _, f := range migrationFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ddl.Write(b)
		ddl.WriteString("\n")
	}

	schema := fmt.Sprintf("c_%d", time.Now().UnixNano())
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

func seeded(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.Seed(context.Background()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return s
}

func TestSeedIsIdempotent(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()

	// A second Seed must not duplicate or overwrite.
	if err := s.Seed(ctx); err != nil {
		t.Fatalf("second Seed: %v", err)
	}
	snaps, srcs, err := s.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snaps != 1 || srcs != 1 {
		t.Errorf("after two seeds: %d snapshot(s), %d source(s); want 1 and 1", snaps, srcs)
	}
}

func TestSeedDoesNotClobberExisting(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A real catalog entry that predates any seeding.
	if err := s.PutSource(ctx, Source{
		ID: "mine", Name: "mine", Version: "1", Kind: "vcf", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	_, srcs, _ := s.Count(ctx)
	if srcs != 1 {
		t.Errorf("Seed added to a non-empty catalog: %d sources", srcs)
	}
}

func TestGetSnapshotIncludesSources(t *testing.T) {
	s := seeded(t)
	snap, err := s.GetSnapshot(context.Background(), "dev")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Build != "GRCh38" || snap.State != StatePublished {
		t.Errorf("unexpected snapshot: %+v", snap)
	}
	if len(snap.Sources) != 1 || snap.Sources[0].Ref() != "builtins:1" {
		t.Fatalf("sources = %+v, want builtins:1", snap.Sources)
	}
	if got := snap.Defaults; len(got) != 3 {
		t.Errorf("defaults = %v, want 3 entries", got)
	}
	if snap.ContainsPrivate() {
		t.Error("builtins is public; ContainsPrivate should be false")
	}
}

func TestGetSnapshotUnknown(t *testing.T) {
	s := seeded(t)
	_, err := s.GetSnapshot(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A snapshot with no pinned sources materializes into a manifest varhub rejects.
// Fail at the catalog with a message that names the real problem.
func TestGetSnapshotRejectsEmpty(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.PutSnapshot(ctx, Snapshot{ID: "empty", Build: "GRCh38"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetSnapshot(ctx, "empty")
	if err == nil || !strings.Contains(err.Error(), "pins no sources") {
		t.Errorf("err = %v, want a 'pins no sources' error", err)
	}
}

func TestPutSnapshotReplacesPins(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "extra", Name: "extra", Version: "2", Kind: "vcf",
		TOML: "[[sources]]\n  name = \"extra\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	// Re-pin with both, in a specific order.
	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "dev", Build: "GRCh38", State: StatePublished,
	}, []string{"extra", "builtins"}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetSnapshot(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(snap.Sources))
	}
	if snap.Sources[0].ID != "extra" || snap.Sources[1].ID != "builtins" {
		t.Errorf("pin order not preserved: %s, %s", snap.Sources[0].ID, snap.Sources[1].ID)
	}

	// Re-pin with one: the removed pin must be gone, not accumulated.
	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "dev", Build: "GRCh38", State: StatePublished,
	}, []string{"builtins"}); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.GetSnapshot(ctx, "dev")
	if len(snap.Sources) != 1 || snap.Sources[0].ID != "builtins" {
		t.Errorf("re-pin did not replace: %+v", snap.Sources)
	}
}

// Deleting a source a snapshot pins would silently change what that snapshot
// means, so the FK is RESTRICT.
func TestPinnedSourceCannotBeDeleted(t *testing.T) {
	s := seeded(t)
	_, err := s.pool.Exec(context.Background(), `DELETE FROM source WHERE id='builtins'`)
	if err == nil {
		t.Fatal("deleting a pinned source should be refused")
	}
	if !strings.Contains(err.Error(), "foreign key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPutSourceValidates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.PutSource(ctx, Source{ID: "x", Name: "x", Version: "1"}); err == nil {
		t.Error("a source with no TOML should be refused")
	}
	if err := s.PutSource(ctx, Source{Name: "x", Version: "1", TOML: "x"}); err == nil {
		t.Error("a source with no id should be refused")
	}
}

func TestMaterializeWritesUsableTree(t *testing.T) {
	s := seeded(t)
	data, cache := t.TempDir(), t.TempDir()
	m := &Materializer{Store: s, DataDir: data, CacheDir: cache, Root: t.TempDir()}

	dir, cleanup, err := m.Home(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Home: %v", err)
	}
	defer cleanup()

	for _, rel := range []string{
		"config.toml",
		"annotations/snapshots/dev.toml",
		"annotations/sources/builtins/1/builtins-1.toml",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}

	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	// data_dir and cache_dir must point at the shared, persistent paths — not
	// inside the ephemeral home, or every job re-downloads its sources.
	if !strings.Contains(string(cfg), data) || !strings.Contains(string(cfg), cache) {
		t.Errorf("config.toml does not reference the shared dirs:\n%s", cfg)
	}
	if strings.Contains(string(cfg), `data_dir         = "./`) {
		t.Error("data_dir is relative to the ephemeral home")
	}

	// The source fragment is stored and written verbatim.
	frag, _ := os.ReadFile(filepath.Join(dir, "annotations/sources/builtins/1/builtins-1.toml"))
	if string(frag) != builtinsTOML {
		t.Errorf("source fragment was rewritten:\n%s", frag)
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("cleanup did not remove the home")
	}
	// The shared dirs must survive cleanup.
	if _, err := os.Stat(data); err != nil {
		t.Errorf("cleanup removed the shared data dir: %v", err)
	}
}

// Admin-supplied strings end up in a generated TOML manifest. A title with a
// quote in it must not be able to produce a file that parses as something else.
func TestMaterializeQuotesHostileStrings(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.PutSource(ctx, Source{
		ID: "b", Name: "builtins", Version: "1", Kind: "builtin", TOML: builtinsTOML,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(ctx, Snapshot{
		ID:    "quoted",
		Title: `evil" ` + "\n" + `sources = ["injected:9"]`,
		Build: "GRCh38",
	}, []string{"b"}); err != nil {
		t.Fatal(err)
	}

	m := &Materializer{Store: s, DataDir: t.TempDir(), CacheDir: t.TempDir(), Root: t.TempDir()}
	dir, cleanup, err := m.Home(ctx, "quoted")
	if err != nil {
		t.Fatalf("Home: %v", err)
	}
	defer cleanup()

	manifest, _ := os.ReadFile(filepath.Join(dir, "annotations/snapshots/quoted.toml"))
	got := string(manifest)
	if strings.Contains(got, `sources     = ["injected:9"]`) {
		t.Fatalf("title injected a sources line:\n%s", got)
	}
	if !strings.Contains(got, `sources     = ["builtins:1"]`) {
		t.Errorf("real sources line missing:\n%s", got)
	}
	// The title must survive as data, escaped onto one line.
	if strings.Count(got, "\ntitle") > 0 || !strings.HasPrefix(got, "title       = ") {
		t.Errorf("unexpected manifest shape:\n%s", got)
	}
}

func TestMaterializeRequiresDirs(t *testing.T) {
	s := seeded(t)
	m := &Materializer{Store: s, Root: t.TempDir()}
	if _, _, err := m.Home(context.Background(), "dev"); err == nil {
		t.Error("Home should require DataDir and CacheDir")
	}
}
