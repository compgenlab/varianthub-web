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
	"../../migrations/0005_adhoc_snapshot.sql",
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
	// A draft: re-pinning is exactly what a draft is for. (Re-pinning a published
	// snapshot is refused — see TestPublishedPinsAreFrozenButMetaIsNot.)
	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "wip", Build: "GRCh38", State: StateDraft,
	}, []string{"builtins"}); err != nil {
		t.Fatal(err)
	}
	// Re-pin with both, in a specific order.
	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "wip", Build: "GRCh38", State: StateDraft,
	}, []string{"extra", "builtins"}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetSnapshot(ctx, "wip")
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
		ID: "wip", Build: "GRCh38", State: StateDraft,
	}, []string{"builtins"}); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.GetSnapshot(ctx, "wip")
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

// Publishing fixes the pinned versions — that is what reproducibility depends
// on. It must not fix the title or the default field selection, or a typo could
// never be corrected.
func TestPublishedPinsAreFrozenButMetaIsNot(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()
	if err := s.PutSource(ctx, Source{
		ID: "other", Name: "other", Version: "1", Kind: "vcf", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}

	// Same pins, published: allowed (this is what publish itself does).
	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "dev", Build: "GRCh38", State: StatePublished,
	}, []string{"builtins"}); err != nil {
		t.Fatalf("re-put with identical pins: %v", err)
	}

	// Different pins on a published snapshot: refused.
	err := s.PutSnapshot(ctx, Snapshot{
		ID: "dev", Build: "GRCh38", State: StatePublished,
	}, []string{"builtins", "other"})
	if !errors.Is(err, ErrPinsFrozen) {
		t.Fatalf("err = %v, want ErrPinsFrozen", err)
	}

	// Metadata and defaults still editable.
	if err := s.UpdateSnapshotMeta(ctx, Snapshot{
		ID: "dev", Title: "Renamed", Defaults: []string{"auto_id"},
	}); err != nil {
		t.Fatalf("UpdateSnapshotMeta: %v", err)
	}
	got, err := s.GetSnapshot(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Renamed" || len(got.Defaults) != 1 {
		t.Errorf("meta not updated: %+v", got)
	}
	// ...and the pins are untouched by a meta edit.
	if len(got.Sources) != 1 || got.Sources[0].ID != "builtins" {
		t.Errorf("meta edit changed pins: %+v", got.Sources)
	}
}

// Publishing fixes what a snapshot means, not that the row exists forever.
func TestPublishedSnapshotCanBeDeleted(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()
	if err := s.DeleteSnapshot(ctx, "dev"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := s.GetSnapshot(ctx, "dev"); !errors.Is(err, ErrNotFound) {
		t.Errorf("snapshot still present: %v", err)
	}
	if err := s.DeleteSnapshot(ctx, "dev"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice should report not-found, got %v", err)
	}
}

// An ad-hoc selection is a real snapshot, but must never appear in a list a
// person picks from — and the same selection must reuse one row.
func TestAdhocSnapshot(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()

	id, err := s.EnsureAdhocSnapshot(ctx, "GRCh38", []string{"builtins"}, []string{"tstv"})
	if err != nil {
		t.Fatalf("EnsureAdhocSnapshot: %v", err)
	}
	again, err := s.EnsureAdhocSnapshot(ctx, "GRCh38", []string{"builtins"}, []string{"tstv"})
	if err != nil || again != id {
		t.Errorf("same selection produced %q then %q", id, again)
	}
	// Order must not matter: the same set is the same selection.
	if a, _ := AdhocID("GRCh38", []string{"a", "b"}), 0; a != AdhocID("GRCh38", []string{"b", "a"}) {
		t.Error("AdhocID depends on order")
	}
	// A different build is a different selection.
	if AdhocID("GRCh37", []string{"builtins"}) == id {
		t.Error("AdhocID ignores the build")
	}

	snaps, err := s.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sn := range snaps {
		if sn.State == StateAdhoc {
			t.Errorf("ad-hoc snapshot %q leaked into the list", sn.ID)
		}
	}
	// It still resolves and materializes like any snapshot.
	full, err := s.GetSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("ad-hoc snapshot does not load: %v", err)
	}
	if len(full.Sources) != 1 || full.Build != "GRCh38" {
		t.Errorf("unexpected ad-hoc snapshot: %+v", full)
	}

	if _, err := s.EnsureAdhocSnapshot(ctx, "", []string{"builtins"}, nil); err == nil {
		t.Error("a build should be required")
	}
	if _, err := s.EnsureAdhocSnapshot(ctx, "GRCh38", nil, nil); err == nil {
		t.Error("at least one source should be required")
	}
}

func TestAnnotationsFromTOML(t *testing.T) {
	anns := AnnotationsFromTOML(builtinsTOML)
	if len(anns) != 3 {
		t.Fatalf("got %d annotations, want 3", len(anns))
	}
	names := map[string]bool{}
	for _, a := range anns {
		names[a.Name] = true
	}
	for _, want := range []string{"auto_id", "tstv", "indel"} {
		if !names[want] {
			t.Errorf("missing %q: %+v", want, anns)
		}
	}
	// A field-bearing source carries its field and type through.
	withField := AnnotationsFromTOML(`[[sources]]
  name = "clinvar"
  [[sources.annotations]]
    name = "clinvar_sig"
    field = "CLNSIG"
    type = "categorical"
    description = "Clinical significance"
`)
	if len(withField) != 1 || withField[0].Field != "CLNSIG" ||
		withField[0].Type != "categorical" || withField[0].Description == "" {
		t.Errorf("field metadata lost: %+v", withField)
	}
	if got := AnnotationsFromTOML("not ][ toml"); got != nil {
		t.Errorf("unparseable manifest should yield no fields, got %+v", got)
	}
}
