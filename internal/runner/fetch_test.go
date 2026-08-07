package runner

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// fastaKind is what decides whether a fetched reference needs recompressing,
// and getting it wrong is expensive: a plain-gzip file treated as BGZF reaches
// a tool and fails there, hours after the fetch that chose it.
func TestFastaKindDistinguishesGzipFromBGZF(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "a.fa")
	if err := os.WriteFile(plain, []byte(">chr1\nACGT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if k, err := fastaKind(plain); err != nil || k != fastaPlain {
		t.Errorf("plain FASTA: kind=%v err=%v", k, err)
	}

	// compress/gzip writes no extra field, which is exactly what Ensembl ships.
	gzp := filepath.Join(dir, "b.fa.gz")
	f, err := os.Create(gzp)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	zw.Write([]byte(">chr1\nACGT\n"))
	zw.Close()
	f.Close()
	if k, err := fastaKind(gzp); err != nil || k != fastaGzip {
		t.Errorf("plain gzip: kind=%v err=%v (must not be mistaken for BGZF)", k, err)
	}
}
