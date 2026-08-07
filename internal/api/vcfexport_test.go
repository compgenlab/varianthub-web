package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedJob inserts a finished job with results, so the export path can be driven
// end to end. Written directly because the queue's own inserter is unexported
// and this package is testing the rendering, not the queue.
func seedJob(t *testing.T, h *harness, id, kind, columns string, rows [][4]any, anns []string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `
		INSERT INTO job (id,kind,snapshot,selection,status,client_ip,created_at,finished_at,columns)
		VALUES ($1,$2,'s','','done','1.1.1.1',1,2,$3)`, id, kind, columns); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := pool.Exec(ctx, `
			INSERT INTO job_variant (job_id,idx,chrom,pos,ref,alt,annotations)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, i, r[0], r[1], r[2], r[3], anns[i]); err != nil {
			t.Fatal(err)
		}
	}
}

const vcfCols = `[
  {"key":"GENE","label":"Gene","type":"text","source":"GENCODE","source_ref":"gencode:48"},
  {"key":"gnomAD-AF","label":"AF","type":"numeric","source":"gnomAD"},
  {"key":"is_coding","label":"Coding","type":"bool","source":"GENCODE"},
  {"key":"note","label":"Note","type":"text","source":"x"}
]`

// The end-to-end shape: a real request through the router produces a VCF whose
// header declares every column and whose records carry the annotations.
func TestExportVCFEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	// Deliberately out of coordinate order in the stored rows.
	seedJob(t, h, "job1", "locus", vcfCols,
		[][4]any{
			{"chr2", int64(500), "C", "T"},
			{"chr1", int64(100), "A", "G"},
		},
		[]string{
			`{"GENE":"BRCA1","gnomAD-AF":0.875,"is_coding":true,"note":"a;b=c"}`,
			`{"GENE":"TP53","gnomAD-AF":12,"is_coding":false,"note":null}`,
		})

	w := h.do("GET", "/api/v1/jobs/job1/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if lines[0] != "##fileformat=VCFv4.2" {
		t.Errorf("first line = %q", lines[0])
	}
	// Every column is declared, with its key sanitised into a legal ID.
	for _, want := range []string{
		`##INFO=<ID=GENE,Number=1,Type=String,`,
		`##INFO=<ID=gnomAD_AF,Number=1,Type=Float,`,
		`##INFO=<ID=is_coding,Number=0,Type=Flag,`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing header %s\n%s", want, out)
		}
	}
	if !strings.Contains(out, "gencode:48") {
		t.Error("a column lost its source attribution in the header")
	}

	var body []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "#") {
			body = append(body, l)
		}
	}
	if len(body) != 2 {
		t.Fatalf("got %d records, want 2:\n%s", len(body), out)
	}

	// Coordinate order, whatever the stored order was: a VCF sorted any other
	// way cannot be indexed, and would look fine until someone ran tabix.
	if !strings.HasPrefix(body[0], "chr1\t100\t") {
		t.Errorf("first record is %q; records are not in coordinate order", body[0])
	}

	first := strings.Split(body[0], "\t")
	if len(first) != 8 {
		t.Fatalf("record has %d columns, want 8: %q", len(first), body[0])
	}
	if first[2] != "." || first[5] != "." || first[6] != "." {
		t.Errorf("ID/QUAL/FILTER should be missing, got %q %q %q", first[2], first[5], first[6])
	}
	info := first[7]
	if !strings.Contains(info, "GENE=TP53") {
		t.Errorf("INFO lost the gene: %q", info)
	}
	if !strings.Contains(info, "gnomAD_AF=12") || strings.Contains(info, "gnomAD_AF=12.0") {
		t.Errorf("a whole number was not rendered as an integer: %q", info)
	}
	// false is absent, not "=false"; null is absent entirely.
	if strings.Contains(info, "is_coding") {
		t.Errorf("a false flag was written: %q", info)
	}
	if strings.Contains(info, "note") {
		t.Errorf("a null annotation was written: %q", info)
	}

	// The other record: a true flag is bare, and a value with separators is
	// encoded so it cannot end the field early.
	second := strings.Split(body[1], "\t")[7]
	if !strings.Contains(second, "is_coding") || strings.Contains(second, "is_coding=") {
		t.Errorf("a true flag should be bare: %q", second)
	}
	if strings.Contains(second, "a;b=c") {
		t.Errorf("a value with separators was written literally: %q", second)
	}
	if !strings.Contains(second, "note=a%3Bb%3Dc") {
		t.Errorf("value not percent-encoded: %q", second)
	}
	// Four annotations are present on this record, so exactly three separators.
	// A higher count would mean an escaped value had ended its field early,
	// which is the failure that parses cleanly into the wrong values.
	if got := strings.Count(second, ";"); got != 3 {
		t.Errorf("INFO has %d separators, want 3 (4 fields): %q", got, second)
	}
}

// A job submitted as a VCF gets a VCF back without having to ask, since the
// caller already had one.
func TestVCFJobDefaultsToVCFOutput(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	seedJob(t, h, "vcfjob", "vcf", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53"}`})
	seedJob(t, h, "locusjob", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53"}`})

	if w := h.do("GET", "/api/v1/jobs/vcfjob/export", tok, nil); !strings.HasPrefix(
		w.Body.String(), "##fileformat=VCFv4.2") {
		t.Errorf("a VCF job did not default to VCF: %.60s", w.Body.String())
	}
	// And a locus job still defaults to JSON, which is what a script expects.
	if w := h.do("GET", "/api/v1/jobs/locusjob/export", tok, nil); !strings.HasPrefix(
		strings.TrimSpace(w.Body.String()), "[") {
		t.Errorf("a locus job did not default to JSON: %.60s", w.Body.String())
	}
}
