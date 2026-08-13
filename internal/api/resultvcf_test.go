package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// storeResultVCF puts a pre-built answer in job storage and points the job's
// result row at it, which is what the worker does when a VCF job finishes.
func storeResultVCF(t *testing.T, h *harness, root, id, body string) string {
	t.Helper()
	p := filepath.Join(root, "jobs", id, queue.ResultName)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// Gzipped, as the worker writes it and as the name says. Storing it plain
	// would test a file the server never produces.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Insert-or-update: the seeded job has result rows but not necessarily a
	// chunk_result row, and an UPDATE that matches nothing would leave vcf_uri
	// unset while looking like it had worked.
	if _, err := pool.Exec(ctx, `
		INSERT INTO chunk_result (chunk_id, vcf_uri) VALUES ($1, $2)
		ON CONFLICT (chunk_id) DO UPDATE SET vcf_uri = excluded.vcf_uri`, chunkOf(id), p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A pre-built answer is served as it is, not merged again.
//
// Asserted by storing something only this test could have written: if the
// export re-derived the file from the input and the rows, that marker could not
// appear. Checking the annotations were present would pass either way.
func TestAPrebuiltResultIsServedWithoutMergingAgain(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedVCFJob(t, h, "prebuilt")

	const marker = "##varianthub_prebuilt=yes"
	storeResultVCF(t, h, root, "prebuilt", marker+"\n"+submittedVCF)

	w := h.do("GET", "/api/v1/jobs/prebuilt/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), marker) {
		t.Errorf("the stored file was not served; the export merged again:\n%s", w.Body.String())
	}
}

// A stored file that has gone missing falls back to merging, rather than
// failing the download.
//
// It is a shortcut, not the only copy: the input and the rows are both still
// there, and an object lost to a botched sweep or a bucket lifecycle rule
// should cost time rather than the answer.
func TestAMissingPrebuiltResultFallsBackToMerging(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedVCFJob(t, h, "lostfile")

	p := storeResultVCF(t, h, root, "lostfile", "##should-not-be-served\n")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	w := h.do("GET", "/api/v1/jobs/lostfile/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	// Merged from the input: the submitter's own fields are back.
	if !strings.Contains(out, "GENE=theirs") || !strings.Contains(out, "rs121913529") {
		t.Errorf("the fallback did not merge the submitted file:\n%s", out)
	}
	if !strings.Contains(out, "KRAS") {
		t.Errorf("the fallback lost the annotations:\n%s", out)
	}
}

// A job with no pre-built file merges, which is every job submitted before this
// existed and every job whose merge did not succeed.
func TestAJobWithNoPrebuiltResultStillExports(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)
	seedVCFJob(t, h, "nostored")

	w := h.do("GET", "/api/v1/jobs/nostored/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GENE=theirs") {
		t.Errorf("no merge happened:\n%s", w.Body.String())
	}
}
