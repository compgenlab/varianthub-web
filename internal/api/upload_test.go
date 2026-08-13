package api

import (
	"bytes"
	"compress/gzip"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/config"
)

// uploadServer is a server whose job storage is a directory this test can look
// inside, so "was the object written" and "was it cleaned up" are answerable.
func uploadServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	h := New(&config.Config{
		AllowAnonymous: true, Version: "test",
		RatePerMin: 1000, RateBurst: 1000,
		JobStorage: dir,
	}, nil, nil, nil, nil).Routes()
	return h, dir
}

// storedFiles lists every object under the job storage root, so a test can
// assert on what is there without knowing the generated job id.
func storedFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// post sends a multipart body whose parts are written in the given order.
//
// Order is the point: our own front-end appends the file first (api.ts), so the
// handler meets the VCF before it knows which snapshot to annotate it against.
func post(t *testing.T, h http.Handler, parts ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	return postAs(t, h, "", parts...)
}

// postAs is post with a bearer token, for the paths that need a credential.
func postAs(t *testing.T, h http.Handler, bearer string, parts ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		name, val := p[0], p[1]
		if name == "vcf" {
			fw, err := mw.CreateFormFile("vcf", "sample.vcf")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write([]byte(val)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := mw.WriteField(name, val); err != nil {
			t.Fatal(err)
		}
	}
	mw.Close()

	r := httptest.NewRequest("POST", "/api/v1/annotate/vcf", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const uploadVCF = "##fileformat=VCFv4.2\n" +
	"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n" +
	"chr1\t100\t.\tA\tT\t50\tPASS\t.\n"

// A file part that is empty is caught, and nothing is stored.
func TestAnEmptyUploadIsRejectedAndStoresNothing(t *testing.T) {
	h, dir := uploadServer(t)

	w := post(t, h, [2]string{"vcf", ""}, [2]string{"snapshot", "s"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if files := storedFiles(t, dir); len(files) != 0 {
		t.Errorf("an empty upload left %v in job storage", files)
	}
}

// A request with no file part is refused whatever else it carries, and the
// error names what is missing.
func TestAnUploadWithNoFilePartIsRefused(t *testing.T) {
	h, _ := uploadServer(t)

	w := post(t, h, [2]string{"snapshot", "s"}, [2]string{"annotations", "all"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "vcf") {
		t.Errorf("the error should name the missing part: %s", w.Body)
	}
}

// Two file parts is a request with no single answer to "what was submitted", so
// it is refused rather than silently taking one.
//
// It is also the reject-after-store path, which is the cost of streaming: the
// first file is already in storage when the second one makes the request
// invalid, so it has to be taken back. If it were not, every such submission
// would leak an object that nothing points at — there is no job row, so a sweep
// keyed on job ids would never find it either.
func TestTwoFilePartsAreRefused(t *testing.T) {
	h, dir := uploadServer(t)

	w := post(t, h,
		[2]string{"vcf", uploadVCF},
		[2]string{"vcf", uploadVCF},
		[2]string{"snapshot", "s"},
	)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if files := storedFiles(t, dir); len(files) != 0 {
		t.Errorf("a refused double upload left %v in job storage", files)
	}
}

// The whole path, on a real queue: the file part arrives first, the metadata
// after it, and the job that comes out points at an object named for what was
// actually uploaded.
//
// Compression is classified from the bytes exactly once, here, and recorded in
// the name — that name is the only place the answer lives afterwards, which is
// why it is what this asserts on. A plain file stored as input.vcf.gz would
// have every later reader trying to gunzip plain text.
func TestAnAcceptedUploadIsStoredUnderItsJobAndNamedForItsCompression(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gzip     bool
		wantName string
	}{
		{"plain", false, "input.vcf"},
		{"gzipped", true, "input.vcf.gz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			h := newHarness(t)
			h.withQueue(t)
			h.server.cfg.JobStorage = dir
			h.http = h.server.Routes()
			_, token := h.admin(t)

			body := uploadVCF
			if tc.gzip {
				var b bytes.Buffer
				zw := gzip.NewWriter(&b)
				if _, err := zw.Write([]byte(uploadVCF)); err != nil {
					t.Fatal(err)
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
				body = b.String()
			}

			// File first, metadata after — the order our own front-end sends.
			w := postAs(t, h.http, token,
				[2]string{"vcf", body},
				[2]string{"snapshot", "s"},
			)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
			}

			files := storedFiles(t, dir)
			if len(files) != 1 {
				t.Fatalf("job storage holds %v, want exactly one object", files)
			}
			if got := filepath.Base(files[0]); got != tc.wantName {
				t.Errorf("stored as %q, want %q", got, tc.wantName)
			}
			// jobs/<job-id>/<name>, so a bucket listing says which job owns
			// what — the layout that makes orphan collection a listing rather
			// than a join against rows that may already be gone.
			rel, err := filepath.Rel(dir, files[0])
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) != 3 || parts[0] != "jobs" {
				t.Fatalf("stored at %q, want jobs/<job-id>/<name>", rel)
			}
			if len(parts[1]) != 32 {
				t.Errorf("the middle segment %q is not a job id", parts[1])
			}

			// And the bytes survived the trip unchanged.
			stored, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(stored) != body {
				t.Errorf("the stored object differs from what was uploaded (%d vs %d bytes)",
					len(stored), len(body))
			}
		})
	}
}
