package enginetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The engine returns records in the order it was given them.
//
// The cache decorator depends on this, and it is what lets a submission keep the
// order it arrived in. The plan had been to sort a locus list into coordinate
// order at submit, so that the cached values and the freshly computed ones could
// be merged by position — which would have meant handing somebody back their
// variants in an order they did not ask for.
//
// It is unnecessary. The reduced file the engine is asked to annotate is the
// input with the cache hits removed, so it is a subsequence of the input in the
// same relative order; walking the two together needs them consistent, not
// sorted. That holds for any input order at all, provided the engine does not
// reorder — which is what this pins.
//
// The one thing an unsorted result costs is a tabix index, and it costs it only
// where it does not matter: a submitted VCF is already coordinate-sorted, so its
// answer is too, while a hand-typed locus list is small enough that nobody
// indexes it.
func TestTheEngineKeepsTheInputsOrder(t *testing.T) {
	h := Build(t, Fixture{
		Annotations: []Annotation{{Name: "sig", Field: "SIG"}},
		Records: []Record{
			{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G", Info: map[string]string{"SIG": "one"}},
			{Chrom: "chr1", Pos: 900, Ref: "T", Alt: "C", Info: map[string]string{"SIG": "nine"}},
			{Chrom: "chr2", Pos: 50, Ref: "C", Alt: "T", Info: map[string]string{"SIG": "two"}},
		},
	})

	// Deliberately not in coordinate order, and not on one contig.
	in := filepath.Join(t.TempDir(), "in.vcf")
	if err := os.WriteFile(in, []byte("##fileformat=VCFv4.2\n"+
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+
		"chr2\t50\t.\tC\tT\t.\t.\t.\n"+
		"chr1\t900\t.\tT\tC\t.\t.\t.\n"+
		"chr1\t100\t.\tA\tG\t.\t.\t.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(h.Bin, "-home", h.Dir, "annotate", "--format", "vcf", in).Output()
	if err != nil {
		t.Fatalf("an unsorted input was refused: %v", err)
	}

	var loci []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		loci = append(loci, f[0]+":"+f[1])
		// And each record still carries its own annotation, so "order kept" is
		// not being satisfied by dropping the annotations.
		if !strings.Contains(f[7], "sig=") {
			t.Errorf("record %s lost its annotation: %q", f[0]+":"+f[1], line)
		}
	}
	want := []string{"chr2:50", "chr1:900", "chr1:100"}
	if strings.Join(loci, ",") != strings.Join(want, ",") {
		t.Errorf("records came back as %v, want %v — the engine reordered them, "+
			"and the cache merge cannot walk two streams that disagree", loci, want)
	}
}
