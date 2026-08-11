package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/anncache"
)

func seedSource(t *testing.T, s *Store, ctx context.Context) Source {
	t.Helper()
	src := Source{
		ID: "vep:113", Name: "vep", Version: "113", Kind: "tool", Build: "GRCh38",
		TOML:        "[[sources]]\n  type = \"tool\"\n  name = \"vep\"\n  version = \"113\"\n",
		IndexStatus: "indexed",
	}
	if err := s.PutSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	return src
}

// The whole point: correcting a manifest must not cost a re-download.
//
// index_status is what the deployment learned by provisioning — hours of it for
// something like VEP — and the manifest does not know it. Registering again
// through the create path takes it from the new draft, so a one-line fix would
// mark the source as not downloaded.
func TestUpdatingAManifestKeepsTheDownloadState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedSource(t, s, ctx)

	next := Source{
		ID: "vep:113", Name: "vep", Version: "113", Kind: "tool", Build: "GRCh38",
		TOML: "[[sources]]\n  type = \"tool\"\n  name = \"vep\"\n  version = \"113\"\n" +
			"  requires_reference = true\n",
		// A fresh draft knows nothing about provisioning.
		IndexStatus: "",
	}
	got, err := s.UpdateSourceTOML(ctx, "vep:113", next)
	if err != nil {
		t.Fatal(err)
	}
	if got.IndexStatus != "indexed" {
		t.Errorf("IndexStatus = %q, want it carried over as \"indexed\" — "+
			"editing a manifest must not mean re-downloading the data", got.IndexStatus)
	}
	if !strings.Contains(got.TOML, "requires_reference") {
		t.Error("the new manifest was not stored")
	}
}

// A named snapshot is a promise about what an annotation ran against.
func TestAPinnedSourceCannotBeRewritten(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := seedSource(t, s, ctx)

	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "prod", Title: "prod", Build: "GRCh38", State: StatePublished,
	}, []string{src.ID}); err != nil {
		t.Fatal(err)
	}

	_, err := s.UpdateSourceTOML(ctx, src.ID, src)
	if err == nil {
		t.Fatal("a source pinned by a published snapshot was rewritten")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("err = %q, want it to name the snapshot holding it", err)
	}
}

// Ad-hoc snapshots must not block. They are regenerable — the same selection
// produces the same id — so counting them would make every source that has ever
// been annotated with uneditable, which is every useful source.
func TestAnAdhocSnapshotDoesNotBlockAnUpdate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := seedSource(t, s, ctx)

	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "adhoc-deadbeef", Title: "adhoc", Build: "GRCh38", State: StateAdhoc,
	}, []string{src.ID}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateSourceTOML(ctx, src.ID, src); err != nil {
		t.Fatalf("an ad-hoc snapshot blocked the update: %v", err)
	}
	// And it is gone, because it described a manifest that no longer exists.
	snaps, err := s.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sn := range snaps {
		if sn.ID == "adhoc-deadbeef" {
			t.Error("the stale ad-hoc snapshot survived the update")
		}
	}
}

// Renaming makes it a different source, and the stored files would belong to
// neither.
func TestAManifestCannotRenameTheSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := seedSource(t, s, ctx)

	renamed := src
	renamed.ID, renamed.Name = "vep:114", "vep"
	renamed.Version = "114"

	_, err := s.UpdateSourceTOML(ctx, src.ID, renamed)
	if err == nil {
		t.Fatal("a manifest renamed the source in place")
	}
	if !strings.Contains(err.Error(), "vep:114") {
		t.Errorf("err = %q, want it to name what the manifest describes", err)
	}
}

// A manifest edit changes which fields a source emits and where each reads from,
// while its name and version — the only things the annotation cache keys on —
// stay exactly as they were. Nothing about the key says the stored values are
// now answers to a different question, so the edit has to say it.
func TestUpdatingAManifestDiscardsThatSourcesCachedValues(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := seedSource(t, s, ctx)

	cache := anncache.New(s.pool)
	locus := anncache.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}
	if err := cache.Put(ctx, "GRCh38", []anncache.Unit{
		{Locus: locus, Source: src.Ref(), Entries: anncache.Hit{"consequence": {Str: "missense"}}},
		{Locus: locus, Source: "gnomad:4.1", Entries: anncache.Hit{"af": {Num: 0.25, IsNum: true}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	next := src
	next.TOML = src.TOML + "  requires_reference = true\n"
	if _, err := s.UpdateSourceTOML(ctx, src.ID, next); err != nil {
		t.Fatal(err)
	}

	hits, err := cache.Lookup(ctx, "GRCh38", []anncache.Locus{locus},
		[]string{src.Ref(), "gnomad:4.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits.Get(locus.Key(), src.Ref()); ok {
		t.Error("values computed from the replaced manifest survived the edit")
	}
	// Only that source. Everything else was computed from a manifest nobody
	// touched and is still correct.
	if _, ok := hits.Get(locus.Key(), "gnomad:4.1"); !ok {
		t.Error("the edit discarded another source's values")
	}
}

// A rejected edit must not have cost anything: the manifest is unchanged, so
// what was computed from it is still the right answer.
func TestARefusedManifestEditKeepsTheCache(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := seedSource(t, s, ctx)

	cache := anncache.New(s.pool)
	locus := anncache.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}
	if err := cache.Put(ctx, "GRCh38", []anncache.Unit{
		{Locus: locus, Source: src.Ref(), Entries: anncache.Hit{"consequence": {Str: "missense"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	renamed := src
	renamed.ID, renamed.Name, renamed.Version = "vep:114", "vep", "114"
	if _, err := s.UpdateSourceTOML(ctx, src.ID, renamed); err == nil {
		t.Fatal("a manifest renamed the source in place")
	}

	hits, err := cache.Lookup(ctx, "GRCh38", []anncache.Locus{locus}, []string{src.Ref()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits.Get(locus.Key(), src.Ref()); !ok {
		t.Error("a refused edit discarded the cache anyway")
	}
}
