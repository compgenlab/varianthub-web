package cacherunner

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// The submitted file survives the cache.
//
// The decorator answers part of a request from stored values and asks the engine
// for the rest, then puts the two together. What it must not do is lose anything
// the caller sent: the samples, their own INFO, the ID and FILTER columns. Those
// are exactly what a sites-only reconstruction would drop, and dropping them is
// invisible in the annotations — which are all anybody would think to check.
func TestTheSubmittedRecordsSurviveASubsetRun(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	const submitted = `##fileformat=VCFv4.2
##INFO=<ID=DP,Number=1,Type=Integer,Description="depth">
##FORMAT=<ID=GT,Number=1,Type=String,Description="Genotype">
#CHROM	POS	ID	REF	ALT	QUAL	FILTER	INFO	FORMAT	tumour	normal
chr1	100	rs1	A	G	40	PASS	DP=88	GT	0/1	0/0
chr1	200	rs2	C	T	50	PASS	DP=12	GT	1/1	0/1
chr2	300	.	G	A	60	q10	DP=7	GT	0/1	0/0
`
	if err := os.WriteFile(in, []byte(submitted), 0o600); err != nil {
		t.Fatal(err)
	}

	// A reduced copy holding only the middle record, as a run that had the
	// other two cached would ask for.
	sub := filepath.Join(dir, "ask.vcf")
	n, err := writeSubset(in, sub, map[string]bool{"chr1:200:C:T": true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("kept %d records, want 1", n)
	}

	body := read(t, sub)
	// The record is there whole — its samples, its own INFO, its ID and FILTER.
	// A sites-only reduction would keep the coordinates and drop all of it, and
	// the engine computes dosage and VAF from those sample columns.
	if !strings.Contains(body, "rs2\tC\tT\t50\tPASS\tDP=12\tGT\t1/1\t0/1") {
		t.Errorf("the reduced record lost part of itself:\n%s", body)
	}
	// And the header came with it, or the engine has no samples to name.
	if !strings.Contains(body, "##FORMAT=<ID=GT") || !strings.Contains(body, "\ttumour\tnormal") {
		t.Errorf("the reduced file lost the header:\n%s", body)
	}
	// The other two are gone; that is the saving.
	for _, gone := range []string{"rs1", "chr2\t300"} {
		if strings.Contains(body, gone) {
			t.Errorf("the reduced file kept %q, so the cache saved nothing", gone)
		}
	}
}

// A multi-allelic record is kept whole when any allele is wanted.
//
// Splitting it would change what the engine sees, and the per-allele decision
// was already made by the lookup.
func TestAMultiAllelicRecordIsKeptWholeOrNotAtAll(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte("##fileformat=VCFv4.2\n"+
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+
		"chr1\t500\t.\tG\tA,C\t.\t.\t.\n"+
		"chr1\t600\t.\tT\tG\t.\t.\t.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "ask.vcf")
	// Only the second allele of the first record is owed an answer.
	if _, err := writeSubset(in, sub, map[string]bool{"chr1:500:G:C": true}); err != nil {
		t.Fatal(err)
	}
	body := read(t, sub)
	if !strings.Contains(body, "chr1\t500\t.\tG\tA,C") {
		t.Errorf("the multi-allelic record was dropped or split:\n%s", body)
	}
	if strings.Contains(body, "chr1\t600") {
		t.Errorf("an unwanted record survived:\n%s", body)
	}
}

// A gzipped input is read as one, because its name says so.
func TestAGzippedInputIsReduced(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.vcf.gz")
	f, err := os.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	zw.Write([]byte("##fileformat=VCFv4.2\n" +
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n" +
		"chr1\t100\t.\tA\tG\t.\t.\t.\n"))
	zw.Close()
	f.Close()

	sub := filepath.Join(dir, "ask.vcf")
	n, err := writeSubset(in, sub, map[string]bool{"chr1:100:A:G": true})
	if err != nil {
		t.Fatalf("a gzipped input was not read: %v", err)
	}
	if n != 1 {
		t.Errorf("kept %d records from a gzipped input, want 1", n)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var _ = io.Discard

// A builtin is never cached, whatever it computes from.
//
// It does not come back under the name the manifest gave it — cghts writes
// CG_TSTV for tstv, and auto_id sets the record ID rather than an INFO field —
// so a freshly computed builtin would be dropped on the way through this
// decorator and a cached one would never be written. The annotation would simply
// be missing from a cached job's answer and present in an uncached one, which is
// the kind of difference nobody goes looking for.
func TestABuiltinIsNeverCacheable(t *testing.T) {
	fields := []catalog.Field{
		{Name: "tstv", SourceRef: "builtins:1", Builtin: "tstv", Manifest: "tstv"},
		{Name: "auto_id", SourceRef: "builtins:1", Builtin: "auto_id", Manifest: "auto_id"},
		{Name: "clinvar_sig", SourceRef: "clinvar:2026-01", Manifest: "CLNSIG"},
	}
	p := &plan{selected: []string{"tstv", "auto_id", "clinvar_sig"}}
	if !p.classify(fields) {
		t.Fatal("classify refused a workable plan")
	}

	for _, ref := range p.sourceRefs() {
		if ref == "builtins:1" {
			t.Error("a builtin source was treated as cacheable; its values would be " +
				"filed under a name this decorator never looks up")
		}
	}
	// The real source is still cached — that is the saving worth keeping.
	var cachesClinvar bool
	for _, ref := range p.sourceRefs() {
		if ref == "clinvar:2026-01" {
			cachesClinvar = true
		}
	}
	if !cachesClinvar {
		t.Error("excluding builtins also excluded the source that costs something")
	}
	// And the builtins are asked of the engine rather than dropped.
	for _, want := range []string{"tstv", "auto_id"} {
		var asked bool
		for _, n := range p.passthrough {
			if n == want {
				asked = true
			}
		}
		if !asked {
			t.Errorf("%q is neither cached nor passed through; it would vanish", want)
		}
	}
}
