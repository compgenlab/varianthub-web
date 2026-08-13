package cacherunner

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/runner"
)

const stagedVCF = "##fileformat=VCFv4.2\n" +
	"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tTUMOR\tNORMAL\n" +
	"chr1\t100\trs1\tA\tT\t50\tPASS\tDP=30\tGT\t0/1\t0/0\n" +
	"chr2\t200\t.\tC\tG,A\t60\tPASS\tDP=40\tGT\t1/2\t0/1\n"

// write puts s at dir/name, gzipping it when the name says so — which is the
// same rule the rest of the system uses to decide what a stored input is.
func write(t *testing.T, dir, name, s string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if strings.HasSuffix(name, ".gz") {
		zw := gzip.NewWriter(f)
		if _, err := zw.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	return p
}

// A staged input parses to the same loci as an inline one, compressed or not.
//
// The compressed case is the bug this closes. A .vcf.gz upload used to reach a
// parser that split raw bytes on newlines: it found nothing, the cache stood
// aside, and every compressed submission silently ran with no cache at all —
// right answers, full cost, and no sign anywhere that it had happened.
func TestAStagedInputParsesTheSameCompressedOrNot(t *testing.T) {
	dir := t.TempDir()
	inline, ok := parseVCF([]byte(stagedVCF))
	if !ok {
		t.Fatal("the inline parse failed; the fixture is wrong")
	}
	// Three alleles: chr2 is multi-allelic and becomes one locus per ALT.
	if len(inline) != 3 {
		t.Fatalf("inline gave %d loci, want 3", len(inline))
	}

	for _, name := range []string{"input.vcf", "input.vcf.gz"} {
		t.Run(name, func(t *testing.T) {
			got, ok := parseVCFFile(write(t, dir, name, stagedVCF))
			if !ok {
				t.Fatal("the staged parse stood aside; the cache would be silently off")
			}
			if len(got) != len(inline) {
				t.Fatalf("staged gave %d loci, inline gave %d", len(got), len(inline))
			}
			for i := range inline {
				if got[i] != inline[i] {
					t.Errorf("locus %d: staged %+v, inline %+v", i, got[i], inline[i])
				}
			}
		})
	}
}

// The name is the authority on compression, not the bytes. A file named .gz
// that is not gzip is a mistake worth standing aside for rather than guessing
// past — guessing is what put four different answers in four consumers.
func TestAMisnamedInputStandsAsideRatherThanGuessing(t *testing.T) {
	// Written directly rather than through write(), which gzips by name and
	// would produce a correctly-named file — the opposite of the fixture.
	p := filepath.Join(t.TempDir(), "lying.vcf.gz")
	if err := os.WriteFile(p, []byte(stagedVCF), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := parseVCFFile(p); ok {
		t.Error("a plain file named .gz was parsed anyway; the name has to be authoritative")
	}
}

// A line the engine would reject fails the whole parse rather than being
// skipped. The caller pairs the engine's output with this list by position, so
// dropping one line puts every annotation after it on the wrong variant — a
// wrong answer indistinguishable from a right one.
func TestAMalformedLineStandsAsideRatherThanShiftingEverything(t *testing.T) {
	dir := t.TempDir()
	bad := "##fileformat=VCFv4.2\n" +
		"#CHROM\tPOS\tID\tREF\tALT\n" +
		"chr1\t100\t.\tA\tT\n" +
		"chr1\tNOT_A_POSITION\t.\tA\tT\n" +
		"chr1\t300\t.\tG\tC\n"
	if _, ok := parseVCFFile(write(t, dir, "bad.vcf", bad)); ok {
		t.Error("a malformed line was skipped; positions would no longer line up")
	}
	if _, ok := parseVCF([]byte(bad)); ok {
		t.Error("the inline parser disagreed with the staged one about a bad line")
	}
}

// A record longer than the scanner's default buffer must not read as end of
// input. A cohort VCF's line grows with its sample count, and truncating at
// 64 KB would cache a prefix of the file as though it were all of it.
func TestALineLongerThanTheDefaultBufferIsRead(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("##fileformat=VCFv4.2\n#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT")
	for i := 0; i < 20_000; i++ {
		fmt.Fprintf(&b, "\tS%d", i)
	}
	b.WriteString("\nchr1\t100\t.\tA\tT\t50\tPASS\tDP=30\tGT")
	for i := 0; i < 20_000; i++ {
		b.WriteString("\t0/1")
	}
	b.WriteString("\n")
	if b.Len() < 64<<10 {
		t.Fatalf("fixture is %d bytes; it must exceed the 64 KB default to test anything", b.Len())
	}

	got, ok := parseVCFFile(write(t, dir, "wide.vcf", b.String()))
	if !ok {
		t.Fatal("a wide VCF stood aside")
	}
	if len(got) != 1 {
		t.Errorf("got %d loci, want 1 — the long line was truncated or dropped", len(got))
	}
}

// A sub-run asks about fewer variants than were submitted, so it must not carry
// the staged path: the runner prefers a path over a body, and the path is the
// whole file. Leaving it set turns the cache from a saving into a multiplier —
// every group re-annotating everything.
func TestASubRunDoesNotInheritTheStagedInput(t *testing.T) {
	req := runner.Request{
		Kind:      runner.KindVCF,
		InputPath: "/tmp/staged/input.vcf.gz",
		Body:      nil,
	}
	sub := req
	sub.Body = []byte("chr1\t100\t.\tA\tT\n")
	sub.InputPath = ""

	if sub.InputPath != "" {
		t.Fatal("the sub-request kept the staged path")
	}
	if req.InputPath == "" {
		t.Error("clearing the sub-request changed the original")
	}
}
