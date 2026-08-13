package api

import (
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// storeJobInputAt writes the submitted VCF into job storage and points the job
// at it, which is how every VCF submitted since job storage landed is held.
//
// gzipped controls both the compression and the stored name, because those are
// the same decision: the name is what every later reader is told, so writing
// gzip under a plain name would be a fixture that no real upload could produce.
func storeJobInputAt(t *testing.T, h *harness, root, id string, gzipped bool, body string) {
	t.Helper()
	name := "input.vcf"
	if gzipped {
		name = "input.vcf.gz"
	}
	p := filepath.Join(root, "jobs", id, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if gzipped {
		zw := gzip.NewWriter(f)
		if _, err := zw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		`INSERT INTO chunk_input (chunk_id, uri) VALUES ($1,$2)`, id, p); err != nil {
		t.Fatal(err)
	}
}

// seedStoredVCFJob is seedVCFJob with the input in job storage rather than in
// Postgres.
func seedStoredVCFJob(t *testing.T, h *harness, root, id string, gzipped bool) {
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
	storeJobInputAt(t, h, root, id, gzipped, submittedVCF)
}

// A VCF whose input lives in job storage exports the same merged file as one
// whose input sits in Postgres — compressed or not.
//
// This is the regression that moving inputs out of the database introduced. The
// export read job_input.body, which a stored job does not have, so it reported
// "no input" and quietly fell through to the rendered-from-rows path: every
// submitted ID, QUAL, FILTER, INFO, FORMAT and sample column dropped, with a
// successful download and no error anywhere. The same silent downgrade the
// gzip handling used to cause, arrived at by a different route.
func TestAStoredInputStillExportsTheSubmittersFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gzipped bool
	}{
		{"plain", false},
		{"gzipped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			h := newHarness(t)
			h.withQueue(t)
			h.server.cfg.JobStorage = root
			h.http = h.server.Routes()
			_, tok := h.admin(t)
			seedStoredVCFJob(t, h, root, "stored"+tc.name, tc.gzipped)

			w := h.do("GET", "/api/v1/jobs/stored"+tc.name+"/export?format=vcf", tok, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("export = %d: %s", w.Code, w.Body.String())
			}
			out := w.Body.String()

			// What the fallback path drops. Each of these is the submitter's own
			// data, and getting a file back without them is the failure.
			for _, want := range []string{
				"##reference=GRCh38",
				"##contig=<ID=chr12,length=133275309>",
				`##FORMAT=<ID=AD,`,
				"rs121913529",
				"60.5",
				"LowQual",
				"DP=120",
				"GENE=theirs",
				"0/1:60,60",
				"0/0:110,0",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("the submitter's %q is missing; this is the sites-only "+
						"fallback, not a merge:\n%s", want, out)
				}
			}
			// And our annotations are there too, or it merged nothing.
			if !strings.Contains(out, "KRAS") {
				t.Errorf("no annotation reached the output:\n%s", out)
			}
		})
	}
}

// An input named .gz that is not gzip stands aside rather than serving
// compressed noise as though it were a VCF. The fallback renders a correct if
// plainer file, which is the better of the two wrong-looking answers.
func TestAMisnamedStoredInputFallsBackRatherThanServingGarbage(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.JobStorage = root
	h.http = h.server.Routes()
	_, tok := h.admin(t)

	// Written plain, stored under a .gz name — the one combination no upload
	// produces, and the one a hand-edited or half-written object would.
	seedJob(t, h, "misnamed", "vcf", `[{"key":"GENE","label":"Gene","type":"text"}]`,
		[][4]any{{"chr12", int64(25245350), "C", "T"}},
		[]string{`{"GENE":"KRAS"}`})
	p := filepath.Join(root, "jobs", "misnamed", "input.vcf.gz")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(submittedVCF), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		`INSERT INTO chunk_input (chunk_id, uri) VALUES ($1,$2)`, "misnamed", p); err != nil {
		t.Fatal(err)
	}

	w := h.do("GET", "/api/v1/jobs/misnamed/export?format=vcf", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	// The fallback: a valid VCF built from rows, carrying our annotation but not
	// the submitter's extras.
	if !strings.Contains(out, "##fileformat=VCF") {
		t.Errorf("the fallback did not produce a VCF:\n%s", out)
	}
	if strings.Contains(out, "GENE=theirs") {
		t.Errorf("the merge ran on bytes it could not decompress:\n%s", out)
	}
}
