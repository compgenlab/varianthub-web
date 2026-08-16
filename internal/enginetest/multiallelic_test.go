package enginetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A multi-allelic record gets each allele's own value, in ALT order.
//
// This is the failure this whole harness was built to catch, and it caught it:
// varhub wrote its values in the order the source returned them, with no allele
// association, so a record with ALTs A,C whose source listed C first came back
// with the values swapped. Well-formed, undetectable by any parser, and wrong.
//
// Asserted here as well as in cghts because this is where the consequence lands
// — a stored result VCF is what a user downloads and what every export is
// converted from, so a value on the wrong allele is an answer this service got
// wrong.
func TestAMultiAllelicRecordKeepsItsAllelesStraight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []Record
		want    string
	}{{
		name: "source order matches ALT order",
		records: []Record{
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "A", Info: map[string]string{"AF": "0.1"}},
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "C", Info: map[string]string{"AF": "0.9"}},
		},
		want: "gnomad_af=0.1,0.9",
	}, {
		// The case that used to swap them.
		name: "source order is reversed",
		records: []Record{
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "C", Info: map[string]string{"AF": "0.9"}},
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "A", Info: map[string]string{"AF": "0.1"}},
		},
		want: "gnomad_af=0.1,0.9",
	}, {
		// The case that reported the second allele's value as the first's.
		name: "only the second allele is known",
		records: []Record{
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "C", Info: map[string]string{"AF": "0.9"}},
		},
		want: "gnomad_af=.,0.9",
	}, {
		name: "only the first allele is known",
		records: []Record{
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "A", Info: map[string]string{"AF": "0.1"}},
		},
		want: "gnomad_af=0.1,.",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			h := Build(t, Fixture{
				Annotations: []Annotation{{Name: "gnomad_af", Field: "AF", Type: "numeric"}},
				Records:     tc.records,
			})
			in := filepath.Join(t.TempDir(), "in.vcf")
			if err := os.WriteFile(in, []byte("##fileformat=VCFv4.2\n"+
				"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+
				"chr1\t500\t.\tG\tA,C\t.\t.\t.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(h.Bin, "-home", h.Dir, "annotate", "--format", "vcf", in).Output()
			if err != nil {
				t.Fatalf("annotate: %v", err)
			}
			got := string(out)
			if !strings.Contains(got, tc.want) {
				t.Errorf("want INFO %q, got:\n%s", tc.want, got)
			}
			// And the header says what the field actually holds.
			if !strings.Contains(got, "ID=gnomad_af,Number=A") {
				t.Errorf("a per-allele field is not declared Number=A:\n%s", got)
			}
		})
	}
}
