package vcfmerge

import (
	"bytes"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

var roundTripCols = []queue.Column{
	{Key: "GENE", Label: "Gene", Type: "categorical", SourceRef: "gencode:48"},
	// A key that is not a legal INFO id, so it is written under a sanitised one
	// and can only be read back through the mapping the writer left.
	{Key: "gnomAD-AF", Label: "AF", Type: "numeric"},
	{Key: "is_coding", Label: "Coding", Type: "flag"},
	{Key: "note", Label: "Note", Type: "text"},
}

// A rendered file reads back as the rows it was rendered from.
//
// This is what makes the stored VCF the primary result rather than one of
// several renderings: every other format is a conversion of it, so a value that
// does not survive the round trip is a value the tab, csv and json exports get
// wrong.
func TestARenderedFileReadsBackAsItsRows(t *testing.T) {
	in := []queue.Variant{{
		Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G",
		Annotations: map[string]any{
			"GENE": "TP53", "gnomAD-AF": 0.125, "is_coding": true,
			// Semicolon and equals end an INFO field early if they are not
			// escaped, so this is the value that turns a wrong answer into a
			// visible one.
			"note": "a;b=c d,e%f",
		},
	}, {
		Chrom: "chr2", Pos: 500, Ref: "C", Alt: "T",
		Annotations: map[string]any{"GENE": "BRCA1", "gnomAD-AF": float64(12)},
	}}

	var buf bytes.Buffer
	if err := Render(&buf, Meta{JobID: "j1"}, roundTripCols, SliceStream(in)); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := readAll(t, buf.String())
	if len(out) != 2 {
		t.Fatalf("read back %d rows, want 2:\n%s", len(out), buf.String())
	}
	for i, want := range in {
		got := out[i]
		if got.Chrom != want.Chrom || got.Pos != want.Pos ||
			got.Ref != want.Ref || got.Alt != want.Alt {
			t.Errorf("row %d locus = %s:%d:%s:%s, want %s:%d:%s:%s", i,
				got.Chrom, got.Pos, got.Ref, got.Alt,
				want.Chrom, want.Pos, want.Ref, want.Alt)
		}
		for k, wv := range want.Annotations {
			gv, ok := got.Annotations[k]
			if !ok {
				t.Errorf("row %d lost %q entirely; the file has %v", i, k, got.Annotations)
				continue
			}
			if gv != wv {
				t.Errorf("row %d %q = %#v, want %#v", i, k, gv, wv)
			}
		}
	}
	// An annotation nothing said anything about stays absent, rather than
	// coming back as the "." the file writes for it.
	if v, ok := out[1].Annotations["note"]; ok {
		t.Errorf("a row with no note came back with note=%#v", v)
	}
	if v, ok := out[1].Annotations["is_coding"]; ok {
		t.Errorf("an unset flag came back as %#v; absent and false are not the same", v)
	}
}

// A number comes back as a number, not as the text of one.
//
// The json export marshals whatever this returns, and the rows it used to read
// came out of Postgres as JSON — so an annotation that is 0.125 from one source
// and "0.125" from the other is the same export disagreeing with itself
// depending on which path served it.
func TestANumericAnnotationSurvivesAsANumber(t *testing.T) {
	var buf bytes.Buffer
	err := Render(&buf, Meta{}, roundTripCols, SliceStream([]queue.Variant{{
		Chrom: "chr1", Pos: 1, Ref: "A", Alt: "T",
		Annotations: map[string]any{"gnomAD-AF": 0.125},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, buf.String())[0].Annotations["gnomAD-AF"]
	if f, ok := got.(float64); !ok || f != 0.125 {
		t.Errorf("gnomAD-AF = %#v (%T), want float64(0.125)", got, got)
	}
}

// A key that had to be sanitised comes back under its own name.
//
// The id in the file is gnomAD_AF; the column is gnomAD-AF. Nothing can turn one
// into the other — an underscore in an id may have been an underscore in the key
// — so the writer records the pair and the reader is told.
func TestASanitisedIDIsReadBackAsItsKey(t *testing.T) {
	var buf bytes.Buffer
	err := Render(&buf, Meta{}, roundTripCols, SliceStream([]queue.Variant{{
		Chrom: "chr1", Pos: 1, Ref: "A", Alt: "T",
		Annotations: map[string]any{"gnomAD-AF": 1.0},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "gnomAD_AF=1") {
		t.Fatalf("expected the value under a sanitised id:\n%s", buf.String())
	}
	ann := readAll(t, buf.String())[0].Annotations
	if _, wrong := ann["gnomAD_AF"]; wrong {
		t.Error("the annotation came back under its INFO id; a caller filtering " +
			"or sorting by the column name would never match it")
	}
	if _, ok := ann["gnomAD-AF"]; !ok {
		t.Errorf("the annotation did not come back under its key: %v", ann)
	}
}

// A file with no mapping uses each INFO id as its own key.
//
// That is varhub's contract rather than a guess: it emits an annotation under
// the name the manifest gave it, so for a file it wrote the id is the key. The
// mapping lines exist for the case where that cannot hold — this service merging
// onto a submitted VCF, where an id may have been sanitised or suffixed to avoid
// colliding with something the submitter already had.
func TestAFileWithoutAMappingUsesItsInfoIDs(t *testing.T) {
	const plain = `##fileformat=VCFv4.2
##INFO=<ID=GENE,Number=A,Type=String,Description="g">
#CHROM	POS	ID	REF	ALT	QUAL	FILTER	INFO
chr1	100	.	A	G	.	.	GENE=TP53
`
	got := readAll(t, plain)
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	if got[0].Annotations["GENE"] != "TP53" {
		t.Errorf("annotations = %v, want GENE read under its own id",
			got[0].Annotations)
	}
}

// A multi-allelic record becomes one row per allele, each with its own value.
//
// The writer emits one comma-separated value per ALT. Reading the field as a
// single value would give both alleles the string "0.1,0.9", which is a number
// column that has quietly stopped being numeric.
func TestAMultiAllelicRecordSplitsIntoARowPerAllele(t *testing.T) {
	const submitted = `##fileformat=VCFv4.2
#CHROM	POS	ID	REF	ALT	QUAL	FILTER	INFO
chr1	100	.	A	G,T	.	.	.
`
	rd, err := vcf.NewVcfReader(strings.NewReader(submitted))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := rd.Header()
	if err != nil {
		t.Fatal(err)
	}
	ann := Annotations{
		VariantKey("chr1", 100, "A", "G"): {"gnomAD-AF": 0.1, "GENE": "TP53"},
		VariantKey("chr1", 100, "A", "T"): {"gnomAD-AF": 0.9},
	}
	var buf bytes.Buffer
	if _, err := Merge(rd, &buf, hdr, roundTripCols, ann); err != nil {
		t.Fatalf("merge: %v", err)
	}

	got := readAll(t, buf.String())
	if len(got) != 2 {
		t.Fatalf("read %d rows from a two-allele record, want 2:\n%s", len(got), buf.String())
	}
	if got[0].Alt != "G" || got[1].Alt != "T" {
		t.Fatalf("alleles = %q, %q, want G and T", got[0].Alt, got[1].Alt)
	}
	if got[0].Annotations["gnomAD-AF"] != 0.1 {
		t.Errorf("first allele AF = %#v, want 0.1", got[0].Annotations["gnomAD-AF"])
	}
	if got[1].Annotations["gnomAD-AF"] != 0.9 {
		t.Errorf("second allele AF = %#v, want 0.9", got[1].Annotations["gnomAD-AF"])
	}
	// The allele that had no gene keeps none: the "." standing in for it in the
	// comma-separated field is the file's way of saying nothing, not a value.
	if got[0].Annotations["GENE"] != "TP53" {
		t.Errorf("first allele gene = %#v, want TP53", got[0].Annotations["GENE"])
	}
	if v, ok := got[1].Annotations["GENE"]; ok {
		t.Errorf("second allele gene = %#v, want none", v)
	}
}

// Escape and Unescape are inverses. Everything above depends on it.
func TestEscapingRoundTrips(t *testing.T) {
	for _, s := range []string{
		"", "plain", "a;b", "k=v", "a,b", "100%", "with space", "tab\there",
		"line\nbreak", "chr1:100", "%3B", "%zz", "trailing%",
	} {
		if got := Unescape(Escape(s)); got != s {
			t.Errorf("Unescape(Escape(%q)) = %q", s, got)
		}
	}
}

func readAll(t *testing.T, text string) []queue.Variant {
	t.Helper()
	var out []queue.Variant
	if err := Rows(strings.NewReader(text), func(v queue.Variant) error {
		out = append(out, v)
		return nil
	}); err != nil {
		t.Fatalf("read back: %v\n%s", err, text)
	}
	return out
}
