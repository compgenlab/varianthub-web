package enginetest

import (
	"os/exec"
	"strings"
	"testing"
)

// The fixture annotates, through a real varhub, against a real tabix index.
//
// This is the harness proving itself. Everything that relies on it — the engine
// swap, the cache rewrite — is only as good as the claim that this home
// actually works, so that claim is checked here rather than assumed by the
// tests that build on it.
func TestTheFixtureAnnotates(t *testing.T) {
	h := Build(t, Fixture{
		Annotations: []Annotation{{Name: "test_sig", Field: "SIG"}},
		Records: []Record{
			{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G", Info: map[string]string{"SIG": "pathogenic"}},
			{Chrom: "chr1", Pos: 200, Ref: "C", Alt: "T", Info: map[string]string{"SIG": "benign"}},
		},
	})

	out, err := exec.Command(h.Bin, "-home", h.Dir, "annotate", "--format", "json",
		"chr1:100:A:G", "chr1:200:C:T", "chr1:300:G:A").CombinedOutput()
	if err != nil {
		t.Fatalf("annotate: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "pathogenic") || !strings.Contains(got, "benign") {
		t.Errorf("the fixture source did not annotate:\n%s", got)
	}
	// A locus the source says nothing about comes back with no value, which is
	// the case a fixture that matched everything would never exercise.
	if strings.Count(got, "pathogenic") != 1 {
		t.Errorf("a value was applied to more than one variant:\n%s", got)
	}
}

// The same fixture through --format vcf, which is the path PR 2 moves to.
//
// Asserted separately from the JSON path because they are different code in
// varhub — the streaming pipeline rather than the engine — and the whole
// question this harness exists to answer is whether they agree.
func TestTheFixtureAnnotatesToVCF(t *testing.T) {
	h := Build(t, Fixture{
		Annotations: []Annotation{{Name: "test_sig", Field: "SIG"}},
		Records: []Record{
			{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G", Info: map[string]string{"SIG": "pathogenic"}},
		},
	})

	out, err := exec.Command(h.Bin, "-home", h.Dir, "annotate", "--format", "vcf",
		"chr1:100:A:G").CombinedOutput()
	if err != nil {
		t.Fatalf("annotate --format vcf: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "##fileformat=VCF") {
		t.Fatalf("not a VCF:\n%s", got)
	}
	if !strings.Contains(got, "test_sig") {
		t.Errorf("the annotation did not reach the VCF's INFO:\n%s", got)
	}
	if !strings.Contains(got, "pathogenic") {
		t.Errorf("the value did not reach the VCF:\n%s", got)
	}
}
