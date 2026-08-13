package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// vcfBody is a format=vcf response as text.
//
// The VCF is served gzipped by every path — it is the stored object handed over
// as it is, and where storage is publicly reachable the caller is redirected to
// that object instead, which can only ever deliver what is stored. A test that
// read the body as text would be asserting against a shape the server no longer
// produces.
func vcfBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if got := w.Header().Get("Content-Type"); got != gzipContentType {
		t.Errorf("Content-Type = %q, want %q", got, gzipContentType)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("the vcf download is not gzip: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the vcf download: %v", err)
	}
	return string(body)
}

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

// pointResultAt records where a job's answer is stored, without writing one.
func pointResultAt(t *testing.T, h *harness, id, uri string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		INSERT INTO chunk_result (chunk_id, vcf_uri) VALUES ($1, $2)
		ON CONFLICT (chunk_id) DO UPDATE SET vcf_uri = excluded.vcf_uri`,
		chunkOf(id), uri); err != nil {
		t.Fatal(err)
	}
}

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

// The VCF download redirects to object storage when the caller can reach it.
//
// This is the whole point of storing the answer as a file: the transfer goes
// from the store to the caller without passing through this service. Relaying a
// chromosome's annotated VCF means reading it out of the store, through this
// process and out again — paid twice, and held open for as long as the client is
// slow.
func TestAVCFDownloadRedirectsToStorageWhenItIsReachable(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)

	seedJob(t, h, "signed", "vcf", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53"}`})
	// The result lives in a bucket, and the deployment has said how the outside
	// world reaches that bucket.
	pointResultAt(t, h, "signed", "s3://results/jobs/signed/"+queue.ResultName)
	blob.RegisterSites([]blob.Site{{
		Name: "results", URI: "s3://results",
		Endpoint: "http://s3:7070", PublicEndpoint: "https://files.example.org",
		Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	}})
	t.Cleanup(func() { blob.RegisterSites(nil) })

	w := h.do("GET", "/api/v1/jobs/signed/export?format=vcf", tok, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("export = %d, want 302 to the object: %s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location does not parse: %v", err)
	}
	if loc.Host != "files.example.org" {
		t.Errorf("redirected to %q, not the public endpoint", loc.Host)
	}
	if loc.Query().Get("X-Amz-Signature") == "" {
		t.Error("the redirect target is unsigned; it is not a capability, just a " +
			"request to a private object")
	}
}

// And relays instead when the store is only reachable from inside.
//
// The same request, the same job, one setting different — so a caller behind a
// deployment with a private gateway sees the file rather than an error, and sees
// the same file the redirect would have delivered.
func TestAVCFDownloadRelaysWhenStorageIsPrivate(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedDivergent(t, h, root, "private")

	// A filesystem-backed result: nothing to sign, so it must be served.
	w := h.do("GET", "/api/v1/jobs/private/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d, want the bytes: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(vcfBody(t, w), "FROMFILE") {
		t.Error("the relayed download did not come from the stored file")
	}
}

// A VCF download is the stored object, and it says so.
//
// It used to be copied to the client verbatim under a filename ending .vcf with
// Content-Type text/plain, so a split job's download was compressed bytes and
// nothing said so. Now the compression is the same on every path and the name
// carries it — which is what lets the redirect to object storage return the same
// file as the relay does.
func TestAVCFDownloadIsTheStoredObjectAndIsNamedAsOne(t *testing.T) {
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
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, ".vcf.gz") {
		t.Errorf("Content-Disposition = %q; a gzipped body under a .vcf name is "+
			"the bug this replaced", cd)
	}
	out := vcfBody(t, w)
	if !strings.HasPrefix(out, "##fileformat=VCFv4.2") {
		t.Errorf("the download is not a VCF:\n%.200s", out)
	}
	if !strings.Contains(out, "FROMFILE") {
		t.Errorf("the download was re-rendered rather than served:\n%s", out)
	}
}
