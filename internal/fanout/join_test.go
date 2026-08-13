package fanout

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/vcf"
)

const chunkHeader = "##fileformat=VCFv4.2\n" +
	"##contig=<ID=chr1,length=248956422>\n" +
	"##INFO=<ID=GENE,Number=1,Type=String,Description=\"Gene\">\n" +
	"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tS1\n"

// chunk builds one annotated chunk: the shared header plus n records starting
// at from.
func chunk(from, n int) string {
	var b strings.Builder
	b.WriteString(chunkHeader)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "chr1\t%d\t.\tA\tT\t50\tPASS\tGENE=G%d\tGT\t0/1\n", from+i, from+i)
	}
	return b.String()
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The whole point: separately-produced chunks join into one file that a VCF
// reader accepts, with one header and every record.
//
// Read back through cghts rather than compared as text, because "is this a
// valid VCF" is the actual question — a concatenation that a reader chokes on
// would pass any assertion about bytes.
func TestJoinedChunksReadBackAsOneVCF(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Chunk 1 keeps its header; the rest are stripped, as the upload does.
	first := writeFile(t, dir, "c1.vcf.gz", gzipped(t, chunk(100, 3)))
	var rest []string
	for i, c := range []string{chunk(200, 3), chunk(300, 4)} {
		var out bytes.Buffer
		n, err := StripHeader(bytes.NewReader(gzipped(t, c)), &out)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("stripping removed the records too")
		}
		rest = append(rest, writeFile(t, dir, fmt.Sprintf("c%d.vcf.gz", i+2), out.Bytes()))
	}

	dest := filepath.Join(dir, "joined.vcf.gz")
	if err := Join(ctx, append([]string{first}, rest...), dest); err != nil {
		t.Fatalf("Join: %v", err)
	}

	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("the joined file is not gzip: %v", err)
	}
	defer gz.Close()

	rd, err := vcf.NewVcfReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := rd.Header()
	if err != nil {
		t.Fatalf("the joined file has no readable header: %v", err)
	}
	if got := hdr.Samples(); len(got) != 1 || got[0] != "S1" {
		t.Errorf("samples = %v, want [S1]", got)
	}

	var positions []int
	for {
		rec, err := rd.NextRecord()
		if err != nil {
			break
		}
		positions = append(positions, rec.Pos)
	}
	// Every record from every chunk, in the order they were cut.
	want := []int{100, 101, 102, 200, 201, 202, 300, 301, 302, 303}
	if len(positions) != len(want) {
		t.Fatalf("got %d records, want %d: %v", len(positions), len(want), positions)
	}
	for i := range want {
		if positions[i] != want[i] {
			t.Fatalf("record %d is at %d, want %d — the join reordered the file",
				i, positions[i], want[i])
		}
	}
}

// Only the first chunk's header survives. A header in the middle of a VCF is
// not something any reader expects, and the ones after the first say nothing
// the first did not.
func TestOnlyTheFirstChunkKeepsItsHeader(t *testing.T) {
	var out bytes.Buffer
	if _, err := StripHeader(bytes.NewReader(gzipped(t, chunk(200, 2))), &out); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "#") {
		t.Errorf("a header line survived stripping:\n%s", body)
	}
	if !strings.Contains(string(body), "chr1\t200") {
		t.Errorf("stripping removed the records:\n%s", body)
	}
}

// A record longer than the scanner's default is carried through whole.
//
// A cohort VCF's line grows with its sample count, and a truncated chunk joins
// into a file that is shorter than it should be while looking entirely valid.
func TestAWideRecordSurvivesStripping(t *testing.T) {
	var b strings.Builder
	b.WriteString(chunkHeader)
	b.WriteString("chr1\t100\t.\tA\tT\t50\tPASS\tGENE=G\tGT")
	for i := 0; i < 30_000; i++ {
		b.WriteString("\t0/1")
	}
	b.WriteString("\n")
	if b.Len() < 64<<10 {
		t.Fatalf("fixture is %d bytes; it must exceed the 64 KB default", b.Len())
	}

	var out bytes.Buffer
	n, err := StripHeader(bytes.NewReader(gzipped(t, b.String())), &out)
	if err != nil {
		t.Fatalf("a wide record failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("kept %d records, want 1", n)
	}
	gz, _ := gzip.NewReader(&out)
	body, _ := io.ReadAll(gz)
	if len(body) < 64<<10 {
		t.Errorf("the record came back as %d bytes; it was truncated", len(body))
	}
}

// A missing chunk fails before anything is written.
//
// Discovering the gap halfway means a prefix of the answer has already been
// uploaded, and a truncated VCF reads as a complete one that happens to stop
// early — the worst available outcome for a file somebody will analyse.
func TestAMissingChunkFailsBeforeWritingAnything(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first := writeFile(t, dir, "c1.vcf.gz", gzipped(t, chunk(100, 2)))
	dest := filepath.Join(dir, "joined.vcf.gz")

	err := Join(ctx, []string{first, filepath.Join(dir, "gone.vcf.gz")}, dest)
	if err == nil {
		t.Fatal("a missing chunk was joined anyway")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("a partial join was left at the destination")
	}
}

// Joining nothing is an error rather than an empty file, which would be a
// batch reporting success with no results.
func TestJoiningNothingIsAnError(t *testing.T) {
	if err := Join(context.Background(), nil, filepath.Join(t.TempDir(), "x.vcf.gz")); err == nil {
		t.Error("joining zero chunks produced a file")
	}
}
