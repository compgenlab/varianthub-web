package catalog

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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
		Selection:  "",
		Body:       []byte("chr1:115256529:T:C"),
		OutputPath: filepath.Join(t.TempDir(), "result.vcf.gz"),
	})
	if err != nil {
		t.Fatalf("annotate from catalog: %v", err)
	}
	if res.N != 1 {
		t.Fatalf("N = %d, want 1", res.N)
	}

	body := readAnnotatedVCF(t, res.VCFPath)
	var data []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.HasPrefix(line, "#") {
			data = append(data, line)
		}
	}
	if len(data) != 1 {
		t.Fatalf("got %d records:\n%s", len(data), body)
	}
	if !strings.HasPrefix(data[0], "chr1\t115256529\t") {
		t.Fatalf("unexpected variant: %q", data[0])
	}
	// The seeded snapshot's defaults are auto_id, tstv, indel. Getting all three
	// proves default_annotations survived the round trip through Postgres.
	//
	// Under the names the manifest gave them. auto_id is the exception by
	// design: an identifier belongs in the ID column.
	f := strings.Split(data[0], "\t")
	if f[2] != "1-115256529-T-C" {
		t.Errorf("auto_id should be the record ID, got %q", f[2])
	}
	if !strings.Contains(data[0], "tstv=TS") {
		t.Errorf("missing default annotation tstv; got %q", data[0])
	}
	// indel is a flag and this variant is a SNV, so its absence is the answer
	// rather than a missing annotation. The json path reported the key with an
	// empty value; a VCF says the same thing by not writing the field.
	if strings.Contains(data[0], "indel_INSERT") || strings.Contains(data[0], "indel_DELETE") {
		t.Errorf("a SNV was flagged as an indel: %q", data[0])
	}
}

// readAnnotatedVCF returns an annotated VCF as text, decompressing it.
func readAnnotatedVCF(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the engine wrote no output: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("the output is not gzip: %v", err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
