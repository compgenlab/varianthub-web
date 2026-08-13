package fanout

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/vcf"
)

// cgkitBin skips the test when the tool is not installed. It is present in the
// worker image; a developer's machine may not have it.
func cgkitBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath(DefaultCgkitBin)
	if err != nil {
		t.Skipf("%s not on PATH; skipping the split tests", DefaultCgkitBin)
	}
	return p
}

// A VCF is cut into chunks of the requested size, each a complete file.
//
// Run against the real tool rather than a stub: what is being checked is that
// the flags are right and the series is named the way the join expects, and a
// stub would only confirm this test's idea of both.
func TestSplitProducesCompleteChunksInOrder(t *testing.T) {
	bin := cgkitBin(t)
	dir := t.TempDir()

	// 10 records, cut into chunks of 4: 4 + 4 + 2.
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(chunk(100, 10)), 0o600); err != nil {
		t.Fatal(err)
	}

	chunks, err := Split(context.Background(), bin, in, SplitBase(dir), 4)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %v", len(chunks), chunks)
	}

	// Every chunk is a standalone VCF carrying the header, which is what lets
	// the first one supply the header for the whole joined file.
	var total int
	var first int
	for i, p := range chunks {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			t.Fatalf("chunk %d is not gzip: %v", i+1, err)
		}
		rd, err := vcf.NewVcfReader(gz)
		if err != nil {
			t.Fatal(err)
		}
		hdr, err := rd.Header()
		if err != nil {
			t.Fatalf("chunk %d has no header: %v", i+1, err)
		}
		if got := hdr.Samples(); len(got) != 1 {
			t.Errorf("chunk %d lost the samples: %v", i+1, got)
		}
		n := 0
		for {
			rec, err := rd.NextRecord()
			if err != nil {
				break
			}
			if i == 0 && n == 0 {
				first = rec.Pos
			}
			n++
		}
		gz.Close()
		f.Close()
		total += n
		if i < 2 && n != 4 {
			t.Errorf("chunk %d holds %d records, want 4", i+1, n)
		}
	}
	if total != 10 {
		t.Errorf("the chunks hold %d records between them, want 10", total)
	}
	// In order: the first chunk starts at the start of the file, which is what
	// makes a byte concatenation produce a sorted result.
	if first != 100 {
		t.Errorf("the first chunk starts at %d, want 100", first)
	}
}

// The series is named the way the join walks it. If these disagree the join
// finds nothing, or worse finds a prefix and calls it the whole file.
func TestSplitNamesTheSeriesTheJoinExpects(t *testing.T) {
	bin := cgkitBin(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(chunk(1, 5)), 0o600); err != nil {
		t.Fatal(err)
	}

	base := SplitBase(dir)
	chunks, err := Split(context.Background(), bin, in, base, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range chunks {
		want := fmt.Sprintf("%s.%d.vcf.gz", base, i+1)
		if p != want {
			t.Errorf("chunk %d is %q, want %q", i+1, p, want)
		}
	}
}

// A malformed input fails with the tool's own message, which names what it
// objected to. "exit status 1" is not an explanation anybody can act on.
func TestSplitReportsWhatTheToolSaid(t *testing.T) {
	bin := cgkitBin(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "bad.vcf")
	if err := os.WriteFile(in, []byte("this is not a VCF\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Split(context.Background(), bin, in, SplitBase(dir), 2)
	if err == nil {
		t.Fatal("a malformed input was split anyway")
	}
	if !strings.Contains(err.Error(), "vcf-split") {
		t.Errorf("the error does not say which step failed: %v", err)
	}
	if strings.TrimSpace(err.Error()) == "vcf-split: exit status 1:" {
		t.Errorf("the tool's own message was dropped: %v", err)
	}
}

// A chunk size of zero would split forever, so it is refused before the tool is
// reached.
func TestSplitRefusesAZeroChunkSize(t *testing.T) {
	if _, err := Split(context.Background(), "", "in.vcf", "base", 0); err == nil {
		t.Error("a zero chunk size was accepted")
	}
}
