package api

import (
	"net/http"
	"strings"
	"testing"
)

const submittedVCF = `##fileformat=VCFv4.2
##reference=GRCh38
##contig=<ID=chr12,length=133275309>
##INFO=<ID=DP,Number=1,Type=Integer,Description="Total depth">
##INFO=<ID=GENE,Number=1,Type=String,Description="The submitter's own gene call">
##FORMAT=<ID=GT,Number=1,Type=String,Description="Genotype">
##FORMAT=<ID=AD,Number=R,Type=Integer,Description="Allelic depths">
#CHROM	POS	ID	REF	ALT	QUAL	FILTER	INFO	FORMAT	TUMOR	NORMAL
chr12	25245350	rs121913529	C	T	60.5	PASS	DP=120;GENE=theirs	GT:AD	0/1:60,60	0/0:110,0
chr12	25245351	.	G	A,T	31.2	LowQual	DP=45	GT:AD	0/1:20,25	0/0:44,1
`

// seedVCFJob stores a finished VCF job together with the file it was submitted
// with, which is what makes a merged export possible.
func seedVCFJob(t *testing.T, h *harness, id string) {
	t.Helper()
	seedJob(t, h, id, "vcf", `[
	  {"key":"GENE","label":"Gene","type":"text","source":"GENCODE","source_ref":"gencode:48"},
	  {"key":"score","label":"Score","type":"numeric","source":"REVEL"},
	  {"key":"coding","label":"Coding","type":"bool","source":"GENCODE"}
	]`,
		[][4]any{
			{"chr12", int64(25245350), "C", "T"},
			{"chr12", int64(25245351), "G", "A"},
			{"chr12", int64(25245351), "G", "T"},
		},
		[]string{
			`{"GENE":"KRAS","score":0.875,"coding":true}`,
			`{"GENE":"AAA","score":0.1,"coding":false}`,
			`{"GENE":"TTT","score":0.2,"coding":true}`,
		})
	storeJobInput(t, h, id, submittedVCF)
}

// A submitted VCF comes back as the submitter's own file, annotated — not a
// skeleton carrying only what this server knows about.
func TestMergedVCFPreservesTheSubmittedFile(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)
	seedVCFJob(t, h, "vcfmerge")

	w := h.do("GET", "/api/v1/jobs/vcfmerge/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()

	// Everything the submitter sent survives. Each of these was dropped by the
	// rendered-from-rows path, which is why this exists.
	for _, want := range []string{
		"##reference=GRCh38",
		"##contig=<ID=chr12,length=133275309>",
		"##FORMAT=<ID=AD",
		"rs121913529",     // ID
		"60.5",            // QUAL
		"PASS", "LowQual", // FILTER
		"DP=120",        // their INFO
		"0/1:60,60",     // sample columns
		"TUMOR\tNORMAL", // sample names on the #CHROM line
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the submitted file lost %q\n%s", want, out)
		}
	}

	var records []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(l, "#") {
			records = append(records, l)
		}
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want the 2 submitted:\n%s", len(records), out)
	}

	// The annotations are added.
	if !strings.Contains(records[0], "KRAS") {
		t.Errorf("the annotation is missing from the record: %q", records[0])
	}
	// A record must be rewritten, not passed through verbatim: Attributes has no
	// back-reference to its record, so without MarkDirty the values are set and
	// then discarded, and the file comes back byte-perfect and useless.
	if !strings.Contains(records[0], "0.875") {
		t.Errorf("a numeric annotation did not reach the record: %q", records[0])
	}
}

// The submitter's own INFO keys are not overwritten by ours.
//
// Their file declares GENE and so does the snapshot. Replacing theirs would
// substitute our value under a name they chose, which is invisible until
// somebody compares the export with what they sent.
func TestMergedVCFDoesNotClobberTheSubmittersFields(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)
	seedVCFJob(t, h, "clash")

	out := h.do("GET", "/api/v1/jobs/clash/export?format=vcf", tok, nil).Body.String()

	if !strings.Contains(out, "GENE=theirs") {
		t.Errorf("the submitter's own GENE value was replaced:\n%s", out)
	}
	if !strings.Contains(out, "KRAS") {
		t.Errorf("our gene call is missing entirely:\n%s", out)
	}
	// Ours is written under a name that does not collide.
	if !strings.Contains(out, "GENE_2=") {
		t.Errorf("the colliding annotation was not renamed:\n%s", out)
	}
	// And both definitions are declared.
	if strings.Count(out, "##INFO=<ID=GENE,") != 1 {
		t.Errorf("the submitter's GENE definition was duplicated or lost:\n%s", out)
	}
}

// A multi-allelic record is annotated per ALT, in ALT order. Writing the first
// allele's value alone would attribute it to every alternate.
func TestMergedVCFHandlesMultipleAlternates(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)
	seedVCFJob(t, h, "multi")

	out := h.do("GET", "/api/v1/jobs/multi/export?format=vcf", tok, nil).Body.String()
	var multi string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "chr12\t25245351\t") {
			multi = l
		}
	}
	if multi == "" {
		t.Fatalf("the multi-allelic record is missing:\n%s", out)
	}
	// A,T in the file; AAA for A and TTT for T, in that order.
	if !strings.Contains(multi, "GENE_2=AAA,TTT") {
		t.Errorf("per-allele values are wrong or not in ALT order: %q", multi)
	}
	if !strings.Contains(multi, "0.1,0.2") {
		t.Errorf("per-allele numeric values are wrong: %q", multi)
	}
}

// The same job exported twice must produce the same bytes. Ranging a map would
// order a record's INFO differently on every run.
func TestMergedVCFIsDeterministic(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)
	seedVCFJob(t, h, "stable")

	first := h.do("GET", "/api/v1/jobs/stable/export?format=vcf", tok, nil).Body.String()
	for i := 0; i < 4; i++ {
		again := h.do("GET", "/api/v1/jobs/stable/export?format=vcf", tok, nil).Body.String()
		if again != first {
			t.Fatalf("export %d differs from the first:\n%s\n---\n%s", i+1, first, again)
		}
	}
}

// With no stored input there is nothing to merge onto, so the download degrades
// to the rendered-from-rows VCF rather than failing.
func TestMergedVCFFallsBackWithoutTheInput(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)
	seedJob(t, h, "noinput", "vcf", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53"}`})

	w := h.do("GET", "/api/v1/jobs/noinput/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Body.String(), "##fileformat=VCFv4.2") {
		t.Errorf("no VCF was produced:\n%.200s", w.Body.String())
	}
}
