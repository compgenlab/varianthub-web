package catalog

import (
	"context"
	"strings"
	"testing"
)

func TestBuildRoundTripAndCounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutBuild(ctx, Build{Name: "GRCh38", Label: "Human GRCh38", SortOrder: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBuild(ctx, Build{Name: "GRCm39", Label: "Mouse", SortOrder: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSource(ctx, Source{
		ID: "g", Name: "g", Version: "1", Kind: "gtf", Build: "GRCh38", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListBuilds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListBuilds = %d builds, want 2", len(got))
	}
	// sort_order decides the picker's order, so assert it rather than assuming
	// the scan order happens to agree.
	if got[0].Name != "GRCm39" || got[1].Name != "GRCh38" {
		t.Errorf("out of sort_order: %q, %q", got[0].Name, got[1].Name)
	}
	if got[1].Sources != 1 {
		t.Errorf("GRCh38 counted %d sources, want 1", got[1].Sources)
	}
	if got[0].Sources != 0 {
		t.Errorf("GRCm39 counted %d sources, want 0", got[0].Sources)
	}

	// Update in place: the same name must not become a second row.
	if err := s.PutBuild(ctx, Build{Name: "GRCh38", Label: "renamed", SortOrder: 2}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListBuilds(ctx)
	if len(got) != 2 || got[1].Label != "renamed" {
		t.Errorf("PutBuild did not update in place: %+v", got)
	}
}

// Removing a build that sources still declare would leave them working but
// unofferable — the picker would stop listing the only build they can be used
// with, with nothing to explain why.
func TestDeleteBuildRefusedWhileInUse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutBuild(ctx, Build{Name: "GRCh38"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSource(ctx, Source{
		ID: "g", Name: "g", Version: "1", Kind: "gtf", Build: "GRCh38", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}

	err := s.DeleteBuild(ctx, "GRCh38")
	if err == nil {
		t.Fatal("DeleteBuild removed a build a source still declares")
	}
	if !strings.Contains(err.Error(), "1 source") {
		t.Errorf("error does not say what is holding it: %v", err)
	}
	if got, _ := s.ListBuilds(ctx); len(got) != 1 {
		t.Errorf("build removed despite the refusal: %+v", got)
	}

	// Once nothing declares it, the same call succeeds.
	if _, _, err := s.DeleteSource(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBuild(ctx, "GRCh38"); err != nil {
		t.Fatalf("DeleteBuild after the source was removed: %v", err)
	}
	if got, _ := s.ListBuilds(ctx); len(got) != 0 {
		t.Errorf("build survived deletion: %+v", got)
	}
}

// A name with whitespace could never match a manifest's assembly, so the record
// would be a build nothing can ever be registered against.
func TestPutBuildRejectsUnmatchableNames(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, name := range []string{"", "   ", "GRCh38 (hg38)"} {
		if err := s.PutBuild(ctx, Build{Name: name}); err == nil {
			t.Errorf("PutBuild(%q) was accepted", name)
		}
	}
}
