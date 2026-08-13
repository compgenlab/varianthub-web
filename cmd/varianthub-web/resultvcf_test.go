package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/runner"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

var resultCols = []runner.Column{
	{Key: "GENE", Label: "Gene", Type: "categorical"},
	{Key: "gnomAD-AF", Label: "AF", Type: "numeric"},
}

const engineJSON = `[
  {"chrom":"chr1","pos":100,"ref":"A","alt":"G",
   "annotations":{"GENE":"TP53","gnomAD-AF":0.5}},
  {"chrom":"chr2","pos":200,"ref":"C","alt":"T",
   "annotations":{"GENE":"BRCA1","gnomAD-AF":0.25}}
]`

// A locus job produces a result file too.
//
// It never had a submitted VCF to merge onto, and for that reason it used to
// produce no stored answer at all — every download re-rendered it from the rows.
// One object per job, whatever it was asked with, is what lets every export be a
// conversion of one file instead of a second rendering.
func TestALocusJobStoresASitesOnlyVCF(t *testing.T) {
	out := writeAndRead(t, "", runner.Result{Variants: []byte(engineJSON), Columns: resultCols})

	if !strings.HasPrefix(out, "##fileformat=VCFv4.2") {
		t.Fatalf("not a VCF:\n%s", out)
	}
	rows := readRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("read %d rows, want 2:\n%s", len(rows), out)
	}
	if rows[0].Chrom != "chr1" || rows[0].Annotations["GENE"] != "TP53" {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].Annotations["gnomAD-AF"] != 0.25 {
		t.Errorf("second row AF = %#v, want 0.25", rows[1].Annotations["gnomAD-AF"])
	}
}

// A submitted file keeps everything the submitter sent.
//
// The whole reason a VCF job is answered with its own file rather than a
// skeleton: someone who sent a two-sample tumour/normal VCF should not get two
// bare loci back.
func TestASubmittedVCFKeepsItsOwnColumns(t *testing.T) {
	const submitted = `##fileformat=VCFv4.2
##INFO=<ID=DP,Number=1,Type=Integer,Description="depth">
##FORMAT=<ID=GT,Number=1,Type=String,Description="Genotype">
#CHROM	POS	ID	REF	ALT	QUAL	FILTER	INFO	FORMAT	tumour	normal
chr1	100	rs1	A	G	40	PASS	DP=88	GT	0/1	0/0
`
	dir := t.TempDir()
	in := filepath.Join(dir, "input.vcf")
	if err := os.WriteFile(in, []byte(submitted), 0o600); err != nil {
		t.Fatal(err)
	}
	out := writeAndRead(t, in, runner.Result{Variants: []byte(engineJSON), Columns: resultCols})

	for _, want := range []string{"rs1", "DP=88", "tumour", "normal", "0/1", "PASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("the submitted file lost %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "GENE=TP53") {
		t.Errorf("the annotations were not added:\n%s", out)
	}
}

// The stored object is BGZF, and its name says it is compressed.
//
// BGZF rather than plain gzip because the result is a VCF, and a VCF this size
// is one somebody will run tabix over. The two are indistinguishable to anything
// that merely decompresses — a BGZF file opens fine with gzip -d — so the only
// check that catches a plain-gzip writer is looking for the block structure
// itself. It is also what lets the fan-out concatenate pieces into an indexable
// whole; see fanout.Join.
func TestAStoredResultIsBGZF(t *testing.T) {
	if !strings.HasSuffix(queue.ResultName, ".gz") {
		t.Fatalf("ResultName = %q, and this test is about what that suffix promises",
			queue.ResultName)
	}
	var buf bytes.Buffer
	err := writeResultVCF(&buf, vcfmerge.Meta{JobID: "j"}, toQueueCols(resultCols), "",
		runner.Result{Variants: []byte(engineJSON), Columns: resultCols})
	if err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) < 18 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("the stored object is not gzip at all: %x", b[:min(8, len(b))])
	}
	// FEXTRA set, carrying the "BC" block-size subfield. That pair is the whole
	// difference between BGZF and gzip.
	if b[3]&0x04 == 0 || b[12] != 'B' || b[13] != 'C' {
		t.Error("the stored result is plain gzip, not BGZF; it cannot be indexed")
	}
	// And it is terminated. Without the EOF block htslib reports the file as
	// truncated even when every record is present.
	if !bytes.HasSuffix(b, []byte{
		0x1f, 0x8b, 0x08, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff,
		0x06, 0x00, 0x42, 0x43, 0x02, 0x00, 0x1b, 0x00, 0x03, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}) {
		t.Error("the stored result does not end with a BGZF EOF block")
	}
}

// writeAndRead runs the worker's writer and returns the decompressed file.
func writeAndRead(t *testing.T, inputPath string, res runner.Result) string {
	t.Helper()
	var buf bytes.Buffer
	meta := vcfmerge.Meta{Version: "test", JobID: "job1", Snapshot: "snap"}
	if err := writeResultVCF(&buf, meta, toQueueCols(res.Columns), inputPath, res); err != nil {
		t.Fatalf("write: %v", err)
	}
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("the stored object is not gzip: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(body)
}

func readRows(t *testing.T, text string) []queue.Variant {
	t.Helper()
	var out []queue.Variant
	if err := vcfmerge.Rows(strings.NewReader(text), func(v queue.Variant) error {
		out = append(out, v)
		return nil
	}); err != nil {
		t.Fatalf("read rows: %v\n%s", err, text)
	}
	return out
}

func toQueueCols(cols []runner.Column) []queue.Column {
	out := make([]queue.Column, len(cols))
	for i, c := range cols {
		out[i] = queue.Column(c)
	}
	return out
}
