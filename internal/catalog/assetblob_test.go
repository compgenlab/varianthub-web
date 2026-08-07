package catalog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeBlobs is an in-memory content-addressed store, with hooks for the two
// ways real storage misbehaves: losing an object and returning the wrong one.
type fakeBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	// corrupt, when set, is returned for any digest instead of what was stored.
	corrupt []byte
	// verify mirrors the real store, which checks before handing bytes back.
	verify bool
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{objects: map[string][]byte{}, verify: true}
}

func (f *fakeBlobs) PutAsset(_ context.Context, digest string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	f.objects[digest] = append([]byte(nil), content...)
	return nil
}

func (f *fakeBlobs) GetAsset(_ context.Context, digest string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	got, ok := f.objects[digest]
	if !ok {
		return nil, errNotStored
	}
	if f.corrupt != nil {
		got = f.corrupt
	}
	if f.verify {
		if err := VerifyAsset(digest, got); err != nil {
			return nil, err
		}
	}
	return got, nil
}

var errNotStored = errors.New("no such object")

// The point of the change: content leaves Postgres, and what comes back out of
// Assets/AssetsFor is byte-identical to what went in.
func TestAssetsRoundTripThroughBlobs(t *testing.T) {
	blobs := newFakeBlobs()
	s := testStore(t).WithAssetBlobs(blobs)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "revel", Name: "revel", Version: "1.3", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	want := []Asset{
		{Name: "convert.py", Content: "#!/usr/bin/env python3\nprint('hi')\n"},
		{Name: "sub/dir.sh", Content: "#!/bin/sh\necho ok\n"},
	}
	if err := s.PutAssets(ctx, "revel", want); err != nil {
		t.Fatal(err)
	}

	// Nothing inline: the database holds the index, storage holds the bytes.
	inline, err := s.InlineAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inline) != 0 {
		t.Errorf("content stayed in Postgres: %+v", inline)
	}
	if len(blobs.objects) != 2 {
		t.Errorf("stored %d objects, want 2", len(blobs.objects))
	}

	got, err := s.Assets(ctx, "revel")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Assets returned %d, want 2", len(got))
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Content != want[i].Content {
			t.Errorf("asset %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	byID, err := s.AssetsFor(ctx, []string{"revel"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byID["revel"]) != 2 || byID["revel"][0].Content != want[0].Content {
		t.Errorf("AssetsFor = %+v", byID["revel"])
	}
}

// Identical content stored twice is one object. That is the reason for
// addressing by digest rather than by source and name.
func TestIdenticalAssetsShareOneObject(t *testing.T) {
	blobs := newFakeBlobs()
	s := testStore(t).WithAssetBlobs(blobs)
	ctx := context.Background()

	script := Asset{Name: "convert.py", Content: "same bytes\n"}
	for _, id := range []string{"a", "b"} {
		if err := s.PutSource(ctx, Source{
			ID: id, Name: id, Version: "1", Kind: "tab", TOML: "[[sources]]\n",
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutAssets(ctx, id, []Asset{script}); err != nil {
			t.Fatal(err)
		}
	}
	if len(blobs.objects) != 1 {
		t.Errorf("two sources with identical content stored %d objects, want 1",
			len(blobs.objects))
	}
	// Both still resolve it.
	for _, id := range []string{"a", "b"} {
		got, err := s.Assets(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(got) != 1 || got[0].Content != script.Content {
			t.Errorf("%s resolved %+v", id, got)
		}
	}
}

// A refusal from storage must surface as an error naming the asset, not as an
// empty script a build step runs to no effect.
//
// This covers the wrapping only. Whether the bytes are actually checked against
// their digest is a property of the real store and is asserted there, in
// assetblob's TestSubstitutedObjectIsRefused — a fake that verifies would prove
// only that the fake verifies.
func TestRefusedAssetNamesTheFile(t *testing.T) {
	blobs := newFakeBlobs()
	s := testStore(t).WithAssetBlobs(blobs)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "revel", Name: "revel", Version: "1", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAssets(ctx, "revel", []Asset{{Name: "convert.py", Content: "trusted\n"}}); err != nil {
		t.Fatal(err)
	}

	blobs.corrupt = []byte("rm -rf /\n")
	got, err := s.Assets(ctx, "revel")
	if err == nil {
		t.Fatalf("a substituted asset was accepted: %+v", got)
	}
	if !strings.Contains(err.Error(), "convert.py") {
		t.Errorf("error does not name the asset: %v", err)
	}
}

// Without storage configured, content stays in the database rather than the
// registration failing — a deployment with no storage location must still work.
func TestAssetsStayInlineWithoutBlobs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "revel", Name: "revel", Version: "1", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAssets(ctx, "revel", []Asset{{Name: "c.py", Content: "inline\n"}}); err != nil {
		t.Fatal(err)
	}
	inline, err := s.InlineAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inline) != 1 {
		t.Fatalf("expected the content to stay in Postgres, got %+v", inline)
	}
	got, err := s.Assets(ctx, "revel")
	if err != nil || len(got) != 1 || got[0].Content != "inline\n" {
		t.Fatalf("Assets = %+v, %v", got, err)
	}

	// And the backfill moves it once storage exists, leaving the content
	// readable through exactly the same call.
	blobs := newFakeBlobs()
	withBlobs := s.WithAssetBlobs(blobs)
	moved, err := withBlobs.BackfillAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Errorf("backfilled %d, want 1", moved)
	}
	if inline, _ := s.InlineAssets(ctx); len(inline) != 0 {
		t.Errorf("content survived the backfill in Postgres: %+v", inline)
	}
	got, err = withBlobs.Assets(ctx, "revel")
	if err != nil || len(got) != 1 || got[0].Content != "inline\n" {
		t.Fatalf("after backfill: %+v, %v", got, err)
	}

	// Re-running finds nothing left to do.
	again, err := withBlobs.BackfillAssets(ctx)
	if err != nil || again != 0 {
		t.Errorf("re-run moved %d, err %v; want 0, nil", again, err)
	}
}

// The backfill must not clear the only copy on the strength of an upload it
// never read back.
func TestBackfillKeepsContentWhenStorageLosesIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "revel", Name: "revel", Version: "1", Kind: "tab", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAssets(ctx, "revel", []Asset{{Name: "c.py", Content: "precious\n"}}); err != nil {
		t.Fatal(err)
	}

	// Storage accepts the write and then has nothing.
	blobs := newFakeBlobs()
	blackHole := &losingBlobs{fakeBlobs: blobs}
	if _, err := s.WithAssetBlobs(blackHole).BackfillAssets(ctx); err == nil {
		t.Fatal("backfill reported success against storage that kept nothing")
	}
	inline, err := s.InlineAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inline) != 1 {
		t.Fatalf("the only copy was destroyed: %+v", inline)
	}
	got, err := s.Assets(ctx, "revel")
	if err != nil || len(got) != 1 || got[0].Content != "precious\n" {
		t.Fatalf("content no longer readable: %+v, %v", got, err)
	}
}

type losingBlobs struct{ *fakeBlobs }

func (l *losingBlobs) PutAsset(context.Context, string, []byte) error { return nil }
