package catalog

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/compgenlab/varianthub-web/internal/runner"
)

// TestCatalogDrivenAnnotation is Chunk 2's acceptance criterion: a snapshot that
// exists *only* as Postgres rows must annotate correctly, with no annotations
// tree anywhere on disk beforehand.
//
// It wires the real pieces together — catalog -> Materializer -> ExecRunner ->
// varhub — because that path is exactly what breaks: a manifest key spelled
// wrong, a directory laid out differently than varhub resolves, a relative path
// that escapes the temp home. None of those show up in a unit test of either
// half alone.
func TestCatalogDrivenAnnotation(t *testing.T) {
	bin := os.Getenv("VHW_TEST_VARHUB")
	if bin == "" {
		t.Skip("VHW_TEST_VARHUB not set; skipping catalog annotation test")
	}
	s := seeded(t) // also skips when VHW_TEST_DATABASE_URL is unset
	ctx := context.Background()

	m := &Materializer{
		Store:    s,
		DataDir:  t.TempDir(),
		CacheDir: t.TempDir(),
		Root:     t.TempDir(),
	}
	r := &runner.ExecRunner{Bin: bin, Home: m, Timeout: 60 * time.Second}

	res, err := r.Annotate(ctx, runner.Request{
		Kind:     runner.KindLocus,
		Snapshot: "dev",
		// Empty selection: exercises the snapshot's default_annotations, which is
		// the manifest key most easily got wrong.
		Selection: "",
		Body:      []byte("chr1:115256529:T:C"),
	})
	if err != nil {
		t.Fatalf("annotate from catalog: %v", err)
	}
	if res.N != 1 {
		t.Fatalf("N = %d, want 1", res.N)
	}

	var got []struct {
		Chrom       string         `json:"chrom"`
		Pos         int64          `json:"pos"`
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.Unmarshal(res.Variants, &got); err != nil {
		t.Fatalf("bad result JSON: %v\n%s", err, res.Variants)
	}
	if len(got) != 1 || got[0].Chrom != "chr1" || got[0].Pos != 115256529 {
		t.Fatalf("unexpected variant: %+v", got)
	}
	// The seeded snapshot's defaults are auto_id, tstv, indel. Getting all three
	// proves default_annotations survived the round trip through Postgres.
	for _, key := range []string{"auto_id", "tstv", "indel"} {
		if _, ok := got[0].Annotations[key]; !ok {
			t.Errorf("missing default annotation %q; got %v", key, got[0].Annotations)
		}
	}
	if got[0].Annotations["auto_id"] != "chr1_115256529_T_C" {
		t.Errorf("auto_id = %v", got[0].Annotations["auto_id"])
	}
	if got[0].Annotations["tstv"] != "TS" {
		t.Errorf("tstv = %v", got[0].Annotations["tstv"])
	}
}

// Each job gets a fresh home, and the shared data/cache dirs are reused rather
// than recreated — otherwise every job would re-download its sources.
func TestMaterializedHomesAreIndependent(t *testing.T) {
	bin := os.Getenv("VHW_TEST_VARHUB")
	if bin == "" {
		t.Skip("VHW_TEST_VARHUB not set")
	}
	s := seeded(t)
	ctx := context.Background()

	data, cache := t.TempDir(), t.TempDir()
	m := &Materializer{Store: s, DataDir: data, CacheDir: cache, Root: t.TempDir()}

	h1, c1, err := m.Home(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	h2, c2, err := m.Home(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Errorf("two jobs got the same home %s; concurrent jobs would collide", h1)
	}

	// Releasing one job's home must not disturb the other's.
	c1()
	if _, err := os.Stat(h2); err != nil {
		t.Errorf("cleanup of one home removed another: %v", err)
	}
	c2()

	for _, d := range []string{data, cache} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("shared dir %s was removed: %v", d, err)
		}
	}
}
