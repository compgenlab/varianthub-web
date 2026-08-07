package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Like the queue tests, these need a real Postgres; see internal/queue for the
// container invocation. Each test gets its own schema.
// allMigrations are every migration, discovered rather than listed.
//
// The list used to be written by hand, so adding a table meant remembering to
// add it here too — and forgetting showed up as `relation "reference" does not
// exist` in whichever test happened to touch it, rather than as anything about
// the list. Globbing means a new migration is exercised by the existing tests
// the moment it lands.
//
// Sorted, because migrations are ordered by their numeric prefix and a later one
// alters what an earlier one created.
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
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping catalog tests")
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

func TestStorageAndFiles(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()

	// Config-declared locations are reconciled, not merged: one dropped from the
	// config must stop being offered, or a download could target a path the
	// deployment no longer mounts.
	if err := s.SyncConfigStorage(ctx, []StorageLocation{
		{ID: "cfg-a", Name: "a", Kind: StoragePath, URI: "/mnt/a", IsDefault: true},
		{ID: "cfg-b", Name: "b", Kind: StoragePath, URI: "/mnt/b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStorage(ctx, StorageLocation{
		ID: "s3", Name: "bucket", Kind: StorageS3, URI: "s3://x/y",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncConfigStorage(ctx, []StorageLocation{
		{ID: "cfg-a", Name: "a", Kind: StoragePath, URI: "/mnt/a", IsDefault: true},
	}); err != nil {
		t.Fatal(err)
	}
	locs, _ := s.ListStorage(ctx)
	ids := map[string]bool{}
	for _, l := range locs {
		ids[l.ID] = true
	}
	if ids["cfg-b"] {
		t.Error("a location removed from the config is still offered")
	}
	if !ids["s3"] {
		t.Error("sync removed a user-added location")
	}

	// Both kinds are usable targets now that varhub provisions to a bucket and
	// reads back from one; the default is simply the first declared.
	def, err := s.DefaultStorage(ctx)
	if err != nil || def.ID != "cfg-a" {
		t.Errorf("DefaultStorage = %+v, %v", def, err)
	}
	if !(StorageLocation{Kind: StorageS3}).Usable() {
		t.Error("S3 should be a usable download target")
	}
	if !(StorageLocation{Kind: StoragePath}).Usable() {
		t.Error("a filesystem path should be a usable download target")
	}
	// An unknown kind still is not, and says why.
	unknown := StorageLocation{Kind: "gopher"}
	if unknown.Usable() {
		t.Error("an unknown storage kind should not be usable")
	}
	if !strings.Contains(unknown.UnusableReason(), "gopher") {
		t.Errorf("UnusableReason does not name the kind: %q", unknown.UnusableReason())
	}

	// Config-managed locations cannot be deleted through the API path: they come
	// back on the next start, so it would only look like it worked.
	if err := s.DeleteStorage(ctx, "cfg-a"); err == nil {
		t.Error("deleting a config-managed location should be refused")
	}
	if err := s.DeleteStorage(ctx, "s3"); err != nil {
		t.Errorf("deleting a user location: %v", err)
	}

	// File records replace rather than merge, so a re-download that yields fewer
	// files does not leave stale ones listed.
	if err := s.ReplaceSourceFiles(ctx, "builtins", "cfg-a", []SourceFile{
		{Path: "builtins/1/a.gz", SizeBytes: 10, ModifiedAt: 1},
		{Path: "builtins/1/b.gz", SizeBytes: 20, ModifiedAt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.SourceFiles(ctx, "builtins", ""); len(f) != 2 {
		t.Fatalf("got %d files, want 2", len(f))
	}
	if err := s.ReplaceSourceFiles(ctx, "builtins", "cfg-a", []SourceFile{
		{Path: "builtins/1/a.gz", SizeBytes: 11, ModifiedAt: 2},
	}); err != nil {
		t.Fatal(err)
	}
	f, _ := s.SourceFiles(ctx, "builtins", "")
	if len(f) != 1 || f[0].SizeBytes != 11 {
		t.Errorf("replace did not clear stale rows: %+v", f)
	}
}

func TestValidateStorage(t *testing.T) {
	for _, bad := range []StorageLocation{
		{ID: "", Name: "n", Kind: StoragePath, URI: "/a"},
		{ID: "i", Name: "n", Kind: StoragePath, URI: "relative/path"},
		{ID: "i", Name: "n", Kind: StorageS3, URI: "/not/s3"},
		{ID: "i", Name: "n", Kind: "ftp", URI: "/a"},
	} {
		if err := ValidateStorage(bad); err == nil {
			t.Errorf("ValidateStorage(%+v) should fail", bad)
		}
	}
	if err := ValidateStorage(StorageLocation{
		ID: "i", Name: "n", Kind: StoragePath, URI: "/mnt/data",
	}); err != nil {
		t.Errorf("valid location rejected: %v", err)
	}
}

// Annotation and provisioning must agree on a directory: varhub reads one cache
// root, so a source downloaded into location A is invisible to a job pointed at
// location B. That mismatch produced "sources not downloaded" for data sitting
// on disk.
func TestStorageForSources(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()
	if err := s.SyncConfigStorage(ctx, []StorageLocation{
		{ID: "a", Name: "a", Kind: StoragePath, URI: "/mnt/a", IsDefault: true},
		{ID: "b", Name: "b", Kind: StoragePath, URI: "/mnt/b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSource(ctx, Source{
		ID: "other", Name: "other", Version: "1", Kind: "vcf", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}

	// Nothing downloaded: the source is simply absent from the map, and the
	// caller falls back to the default so varhub reports the absence itself.
	roots, err := s.StorageRootsForSources(ctx, []string{"builtins"})
	if err != nil || len(roots) != 0 {
		t.Errorf("undownloaded = %v, %v; want empty", roots, err)
	}

	if err := s.ReplaceSourceFiles(ctx, "builtins", "a", []SourceFile{
		{Path: "builtins/1/x", SizeBytes: 1, ModifiedAt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	roots, err = s.StorageRootsForSources(ctx, []string{"builtins"})
	if err != nil || roots["builtins"] != "/mnt/a" {
		t.Errorf("roots = %v, %v; want builtins at /mnt/a", roots, err)
	}

	// Split across locations is normal, not an error: each source is read where
	// it is, via a location overlay for whichever ones are not at cache_dir.
	if err := s.ReplaceSourceFiles(ctx, "other", "b", []SourceFile{
		{Path: "other/1/y", SizeBytes: 1, ModifiedAt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	roots, err = s.StorageRootsForSources(ctx, []string{"builtins", "other"})
	if err != nil {
		t.Fatalf("split locations should not be an error: %v", err)
	}
	if roots["builtins"] != "/mnt/a" || roots["other"] != "/mnt/b" {
		t.Errorf("roots = %v; want builtins at /mnt/a and other at /mnt/b", roots)
	}
}

// A builtin computes from the variant and has nothing on disk, so it is usable
// the moment it is registered. Treating it as unprovisioned offers a download
// that fetches nothing and then reports success.
func TestBuiltinNeedsNoData(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want bool
	}{
		{"builtin", false},
		{"vcf", true},
		{"bed", true},
		{"gtf", true},
		{"genelist", true}, // needs its genes_file and the GTF it references
		{"tool", true},     // needs an image acquire + one-time setup
	} {
		if got := (Source{Kind: tc.kind}).NeedsData(); got != tc.want {
			t.Errorf("Source{Kind:%q}.NeedsData() = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// A pinned source cannot be removed: doing so would silently change what those
// snapshots mean. Unpinned, it goes — and the caller learns where its files were
// so the bytes can be reclaimed.
func TestDeleteSource(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()

	_, _, err := s.DeleteSource(ctx, "builtins")
	if !errors.Is(err, ErrSourcePinned) {
		t.Fatalf("err = %v, want ErrSourcePinned", err)
	}
	if !strings.Contains(err.Error(), "dev") {
		t.Errorf("error should name the pinning snapshot, got %q", err)
	}

	if err := s.PutSource(ctx, Source{
		ID: "loose", Name: "loose", Version: "1", Kind: "vcf", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncConfigStorage(ctx, []StorageLocation{
		{ID: "a", Name: "a", Kind: StoragePath, URI: "/mnt/a", IsDefault: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceSourceFiles(ctx, "loose", "a", []SourceFile{
		{Path: "loose/1/x.gz", SizeBytes: 5, ModifiedAt: 1},
	}); err != nil {
		t.Fatal(err)
	}

	src, locs, err := s.DeleteSource(ctx, "loose")
	if err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if src.Ref() != "loose:1" {
		t.Errorf("returned %+v", src)
	}
	if len(locs) != 1 || locs[0].URI != "/mnt/a" {
		t.Fatalf("locations = %+v, want the location holding its files", locs)
	}
	// The file records go with the row, so nothing is left pointing at a source
	// that no longer exists.
	if f, _ := s.SourceFiles(ctx, "loose", ""); len(f) != 0 {
		t.Errorf("%d orphaned file rows", len(f))
	}
	if _, _, err := s.DeleteSource(ctx, "loose"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

// An ad-hoc snapshot is generated per submission and hidden from every listing.
// If it blocked deletion, one ad-hoc annotation would make a source permanently
// undeletable — blocked by something the user cannot see or remove.
func TestAdhocSnapshotDoesNotBlockSourceDeletion(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()
	if err := s.PutSource(ctx, Source{
		ID: "loose", Name: "loose", Version: "1", Kind: "vcf", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	adhoc, err := s.EnsureAdhocSnapshot(ctx, "GRCh38", []string{"loose"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pinned, err := s.SourceSnapshots(ctx, "loose"); err != nil || len(pinned) != 0 {
		t.Fatalf("SourceSnapshots = %v, %v; ad-hoc rows must not count", pinned, err)
	}
	if _, _, err := s.DeleteSource(ctx, "loose"); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	// The ad-hoc snapshot goes with it rather than dangling.
	if _, err := s.GetSnapshot(ctx, adhoc); !errors.Is(err, ErrNotFound) {
		t.Errorf("ad-hoc snapshot survived: %v", err)
	}
}

// A wrong assembly does not error at annotate time — it returns plausible
// answers at coordinates that mean something else. So it has to be refused
// where snapshots are built, and that means every path: creating one, editing
// one, and the ad-hoc snapshot an individual-source selection mints. Checking
// it in a single handler left the other two accepting a mix.
func TestAssemblyInvariantOnEveryPath(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()
	for _, src := range []Source{
		{ID: "h38", Name: "h38", Version: "1", Kind: "vcf", Build: "GRCh38", TOML: "[[sources]]\n"},
		{ID: "h37", Name: "h37", Version: "1", Kind: "vcf", Build: "GRCh37", TOML: "[[sources]]\n"},
		{ID: "agnostic", Name: "agnostic", Version: "1", Kind: "builtin", TOML: "[[sources]]\n"},
	} {
		if err := s.PutSource(ctx, src); err != nil {
			t.Fatal(err)
		}
	}

	// Creating one.
	err := s.PutSnapshot(ctx, Snapshot{ID: "mixed", Build: "GRCh38"}, []string{"h38", "h37"})
	if err == nil {
		t.Error("PutSnapshot accepted a mixed-assembly snapshot")
	} else if !strings.Contains(err.Error(), "h37") {
		t.Errorf("error does not name the offender: %v", err)
	}

	// The ad-hoc snapshot an individual-source selection mints.
	if _, err := s.EnsureAdhocSnapshot(ctx, "GRCh38", []string{"h38", "h37"}, nil); err == nil {
		t.Error("EnsureAdhocSnapshot accepted a mixed-assembly selection")
	}

	// Editing one.
	if err := s.PutSnapshot(ctx, Snapshot{ID: "ok", Build: "GRCh38"}, []string{"h38"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSnapshotSources(ctx, "ok", []string{"h38", "h37"}); err == nil {
		t.Error("SetSnapshotSources accepted a mixed-assembly edit")
	}

	// A source with no declared assembly is agnostic and belongs anywhere.
	if _, err := s.SetSnapshotSources(ctx, "ok", []string{"h38", "agnostic"}); err != nil {
		t.Errorf("an assembly-agnostic source was refused: %v", err)
	}
}

// A source registered without a stated visibility is private. The two mistakes
// are not symmetric: publishing something private is a disclosure that cannot
// be undone, while a private source nobody can see is a support request.
func TestSourceDefaultsToPrivate(t *testing.T) {
	s := seeded(t)
	ctx := context.Background()
	if err := s.PutSource(ctx, Source{
		ID: "unstated", Name: "unstated", Version: "1", Kind: "vcf", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSource(ctx, "unstated")
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != VisibilityPrivate {
		t.Errorf("visibility = %q, want private", got.Visibility)
	}
	// A stated visibility is honoured.
	if err := s.PutSource(ctx, Source{
		ID: "stated", Name: "stated", Version: "1", Kind: "vcf",
		Visibility: VisibilityPublic, TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSource(ctx, "stated"); got.Visibility != VisibilityPublic {
		t.Errorf("a stated public visibility was overridden: %q", got.Visibility)
	}
	// The starter catalog must remain usable: its only source is a builtin that
	// computes from the variant and discloses nothing.
	if b, err := s.GetSource(ctx, "builtins"); err != nil || b.Visibility != VisibilityPublic {
		t.Errorf("seeded builtins = %q, %v; want public", b.Visibility, err)
	}
}
