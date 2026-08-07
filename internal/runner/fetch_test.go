package runner

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// faidx accepts plain or bgzip and nothing else, so a plain-gzip FASTA has to be
// normalized before a tool sees it.
//
// Ensembl publishes the GRCh38 primary assembly as plain gzip. Handed straight
// to VEP it fails with "Cannot index files compressed with gzip, please use
// bgzip" from inside the container — a long way from the fetch that chose it.
func TestNormalizeFastaDecompressesPlainGzip(t *testing.T) {
	dir := t.TempDir()
	body := ">chr1\nACGTACGTAC\n"

	gzPath := filepath.Join(dir, "ref.fa.gz")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	if err := os.WriteFile(gzPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := NormalizeFasta(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(got, ".gz") {
		t.Fatalf("still compressed: %s", got)
	}
	out, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != body {
		t.Errorf("content changed: %q", out)
	}
	// The original is redundant and a reference is large enough that keeping
	// both is a real cost.
	if _, err := os.Stat(gzPath); !os.IsNotExist(err) {
		t.Error("the compressed original was left behind")
	}
}

// An uncompressed FASTA is already indexable and must be left alone.
func TestNormalizeFastaLeavesPlainAlone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ref.fa")
	if err := os.WriteFile(p, []byte(">chr1\nACGT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeFasta(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Errorf("path changed: %s -> %s", p, got)
	}
}
