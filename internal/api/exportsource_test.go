package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A file whose values differ from the rows in Postgres, so an export says which
// of the two it read. Checking that the annotations are present would pass
// either way, which is the mistake that would let this whole change be a no-op.
const storedFileVCF = `##fileformat=VCFv4.2
##INFO=<ID=GENE,Number=A,Type=String,Description="Gene",Source="VariantHub">
##varianthub_column=<ID=GENE,Key=GENE>
##INFO=<ID=gnomAD_AF,Number=A,Type=Float,Description="AF",Source="VariantHub">
##varianthub_column=<ID=gnomAD_AF,Key=gnomAD-AF>
#CHROM	POS	ID	REF	ALT	QUAL	FILTER	INFO
chr1	100	.	A	G	.	.	GENE=FROMFILE;gnomAD_AF=0.5
`

// seedDivergent stores a result file that disagrees with the rows, and returns
// the job id.
func seedDivergent(t *testing.T, h *harness, root, id string) string {
	t.Helper()
	seedJob(t, h, id, "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"FROMROWS","gnomAD-AF":0.5,"is_coding":true,"note":null}`})
	storeResultVCF(t, h, root, id, storedFileVCF)
	return id
}

// A tab export is a conversion of the stored file.
//
// The point of making the VCF primary: one answer, converted, rather than a
// second rendering from a copy of the same data. Two renderings are two things
// that can disagree, and the one that drifts is whichever is exercised least.
func TestATabExportIsAConversionOfTheStoredFile(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedDivergent(t, h, root, "conv")

	w := h.do("GET", "/api/v1/jobs/conv/export?format=tsv", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, "FROMFILE") {
		t.Errorf("the tab export was rendered from the rows, not converted from "+
			"the stored file:\n%s", out)
	}
	// The header still comes from the column model, so a column the file had
	// nothing to say about is still a column.
	if !strings.HasPrefix(out, "chrom\tpos\tref\talt\tGENE\tgnomAD-AF\tis_coding\tnote") {
		t.Errorf("unexpected header line:\n%s", out)
	}
}

// So is a json export, and its numbers are still numbers.
//
// The rows came out of Postgres as JSON, where 0.5 is a number. Read back out of
// a VCF it is text unless the INFO type is honoured — and an export that emits
// "0.5" for one job and 0.5 for another is one a consumer cannot parse twice.
func TestAJSONExportFromTheFileKeepsItsTypes(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedDivergent(t, h, root, "typed")

	w := h.do("GET", "/api/v1/jobs/typed/export?format=json", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	var rows []struct {
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("%v\n%s", err, w.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %s", len(rows), w.Body.String())
	}
	if rows[0].Annotations["GENE"] != "FROMFILE" {
		t.Errorf("json export did not come from the stored file: %v", rows[0].Annotations)
	}
	if af, ok := rows[0].Annotations["gnomAD-AF"].(float64); !ok || af != 0.5 {
		t.Errorf("gnomAD-AF = %#v, want the number 0.5", rows[0].Annotations["gnomAD-AF"])
	}
}

// A search is answered from the rows, because the file cannot answer it.
//
// Not a fallback in the apologetic sense — it is the query engine doing the
// query. The file has no index, and filtering it would mean reading all of it,
// which is what streaming exists to avoid.
func TestASearchIsAnsweredFromTheRows(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedDivergent(t, h, root, "searched")

	w := h.do("GET", "/api/v1/jobs/searched/export?format=tsv&q=FROMROWS", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, "FROMROWS") {
		t.Errorf("a filtered export did not come from the rows:\n%s", out)
	}
	// And a filter that matches nothing returns nothing, rather than quietly
	// serving the whole file because the filter could not be applied to it.
	w = h.do("GET", "/api/v1/jobs/searched/export?format=tsv&q=nothingmatchesthis", tok, nil)
	if strings.Contains(w.Body.String(), "chr1") {
		t.Errorf("a filter matching nothing returned rows:\n%s", w.Body.String())
	}
}

// A VCF download is the stored object, decompressed.
//
// It is stored gzipped, and it used to be copied to the client verbatim under a
// filename ending .vcf with Content-Type text/plain — so a split job's download
// was compressed bytes and nothing said so.
func TestAVCFDownloadIsNotServedCompressed(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedDivergent(t, h, root, "plain")

	w := h.do("GET", "/api/v1/jobs/plain/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.Bytes()
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		t.Fatal("the download starts with the gzip magic number; the stored " +
			"object was served without being decompressed")
	}
	if !strings.HasPrefix(string(body), "##fileformat=VCFv4.2") {
		t.Errorf("the download is not a VCF:\n%.200s", body)
	}
	if !strings.Contains(string(body), "FROMFILE") {
		t.Errorf("the download was re-rendered rather than served:\n%s", body)
	}
}
