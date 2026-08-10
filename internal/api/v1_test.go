package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/identity"
)

func TestSelectionNormalization(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
		err  bool
	}{
		{"omitted means snapshot defaults", nil, "", false},
		{"all", "all", "all", false},
		{"comma string passes through", "clinvar_sig,gnomad_af", "clinvar_sig,gnomad_af", false},
		{"array is joined", []any{"a", "b"}, "a,b", false},
		{"array drops blanks", []any{"a", "  ", "b"}, "a,b", false},
		{"string is trimmed", "  all  ", "all", false},
		{"non-string array element", []any{"a", 3}, "", true},
		{"wrong type", 42, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selection(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("selection = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitForClamping(t *testing.T) {
	s := New(&config.Config{SubmitWaitCap: 10 * time.Second}, nil, nil, nil, nil)
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{"2s", 2 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"99", 10 * time.Second}, // clamped to the cap
		{"5m", 10 * time.Second}, // clamped to the cap
		{"-1", 0},                // negative is no wait, not an error
		{"nonsense", 0},          // unparseable is no wait, not a 400
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/api/v1/annotate?wait="+tc.raw, nil)
		if got := s.waitFor(r); got != tc.want {
			t.Fatalf("wait=%q -> %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// History scoping reads the resolved caller, not the request.
//
// It used to read an X-Varhub-Session header the browser wrote for itself. That
// scoped a history but proved nothing, and being indistinguishable from a value
// anyone could send is what let a request with no credential look like a
// visitor. A self-asserted value now scopes nothing at all.
func TestSessionOf(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	if got := sessionOf(r); got != "" {
		t.Fatalf("no session should be empty, got %q", got)
	}

	// The value the old contract honoured, now ignored.
	r.Header.Set("X-Varhub-Session", "abc")
	r.AddCookie(&http.Cookie{Name: "varhub_session", Value: "cookie-sess"})
	if got := sessionOf(r); got != "" {
		t.Fatalf("a self-asserted session still scopes results: %q", got)
	}

	// Only what the server issued and resolved onto the caller counts.
	withCaller := r.WithContext(context.WithValue(r.Context(), callerKey{},
		identity.Caller{AnonSession: "server-issued"}))
	if got := sessionOf(withCaller); got != "server-issued" {
		t.Fatalf("resolved session = %q, want server-issued", got)
	}
}

// openServer is a server with auth off, so handler logic can be exercised
// directly. The queue is nil: every case below must be rejected before the
// handler would touch it.
func openServer(t *testing.T) http.Handler {
	t.Helper()
	return New(&config.Config{
		AllowAnonymous: true, Version: "test",
		RatePerMin: 1000, RateBurst: 1000, SubmitWaitCap: time.Second,
	}, nil, nil, nil, nil).Routes()
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAnnotateValidation(t *testing.T) {
	h := openServer(t)

	t.Run("empty variants", func(t *testing.T) {
		w := postJSON(t, h, "/api/v1/annotate", map[string]any{"snapshot": "s"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
		}
	})

	t.Run("blank variants only", func(t *testing.T) {
		w := postJSON(t, h, "/api/v1/annotate", map[string]any{
			"snapshot": "s", "variants": []string{"  ", ""},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/v1/annotate", strings.NewReader("{nope"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	// An ad-hoc source list must be refused, not silently annotated under
	// something else: a caller who asked for two sources and got a snapshot's
	// defaults would have no way to tell.
	t.Run("ad-hoc sources refused", func(t *testing.T) {
		w := postJSON(t, h, "/api/v1/annotate", map[string]any{
			"sources": []string{"clinvar"}, "variants": []string{"chr17-7676154-C-T"},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "snapshot") {
			t.Fatalf("error should point at `snapshot`: %s", w.Body)
		}
	})

	// Loci reach the engine as argv, so an oversized batch must be rejected with
	// a clear limit rather than failing later on ARG_MAX.
	t.Run("over the variant cap", func(t *testing.T) {
		many := make([]string, maxVariantsPerRequest+1)
		for i := range many {
			many[i] = fmt.Sprintf("chr1-%d-A-T", i+1)
		}
		w := postJSON(t, h, "/api/v1/annotate", map[string]any{
			"snapshot": "s", "variants": many,
		})
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %s", w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "annotate/vcf") {
			t.Fatalf("error should redirect to the VCF path: %s", w.Body)
		}
	})

	t.Run("bad annotations type", func(t *testing.T) {
		w := postJSON(t, h, "/api/v1/annotate", map[string]any{
			"snapshot": "s", "variants": []string{"chr17-7676154-C-T"}, "annotations": 42,
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
		}
	})
}

func TestAnnotateVCFRequiresFilePart(t *testing.T) {
	h := openServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("snapshot", "s")
	mw.Close()

	r := httptest.NewRequest("POST", "/api/v1/annotate/vcf", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "vcf") {
		t.Fatalf("error should name the missing part: %s", w.Body)
	}
}

func TestAnnotateVCFRejectsEmptyFile(t *testing.T) {
	h := openServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("vcf", "empty.vcf")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(nil)
	mw.Close()

	r := httptest.NewRequest("POST", "/api/v1/annotate/vcf", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
}

// A nil catalog must degrade to 503 on the catalog endpoints rather than panic:
// submission and job polling do not depend on it.
func TestCatalogEndpointsWithoutCatalog(t *testing.T) {
	h := openServer(t)
	for _, path := range []string{"/api/v1/snapshots", "/api/v1/snapshots/x", "/api/v1/sources"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503", path, w.Code)
		}
	}
}

// Every route below /api/v1 must require an identity by default.
func TestV1RoutesRequireAnIdentity(t *testing.T) {
	h := New(&config.Config{
		Version: "test", RatePerMin: 1000, RateBurst: 1000,
	}, nil, nil, nil, nil).Routes()

	paths := []struct{ method, path string }{
		{"GET", "/api/v1/snapshots"},
		{"GET", "/api/v1/snapshots/x"},
		{"GET", "/api/v1/sources"},
		{"POST", "/api/v1/annotate"},
		{"POST", "/api/v1/annotate/vcf"},
		{"GET", "/api/v1/jobs"},
	}
	for _, p := range paths {
		r := httptest.NewRequest(p.method, p.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401 when anonymous", p.method, p.path, w.Code)
		}
	}

	// Reading one job by id is deliberately not on that list. An anonymous
	// result's link is its credential, so these routes carry no blanket
	// requirement and authorize against the job itself — which for a job that
	// does not exist, or one belonging to an account, is a 404 rather than a
	// 401. The distinction matters: 401 says "identify yourself and try again",
	// and for a link that is either wrong or not yours, there is nothing to
	// try.
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/v1/jobs/x"},
		{"GET", "/api/v1/jobs/x/export"},
	} {
		r := httptest.NewRequest(p.method, p.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s %s = 401; a shared link must not need a second credential",
				p.method, p.path)
		}
	}
}

// resolved runs a request through the identity middleware, which is the only
// thing that populates the caller — calling trustedCaller on a bare request
// would test nothing but the zero value.
func resolved(s *Server, r *http.Request) (trusted, throttled bool) {
	s.withCaller(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) {
		trusted, throttled = s.trustedCaller(rr), s.throttled(rr)
	})).ServeHTTP(httptest.NewRecorder(), r)
	return
}

// There is no deployment-wide key left to trust. Nothing a caller can put in an
// Authorization header, on a server with no identity store, makes them anything
// other than an anonymous — and therefore throttled — stranger.
func TestNoSharedSecretIsTrusted(t *testing.T) {
	s := New(&config.Config{}, nil, nil, nil, nil)

	for _, bearer := range []string{
		"",
		"garbage",
		// The exact shape the retired master key had, in case a deployment is
		// still sending one.
		"eyJzdWIiOiJ2YXJodWIiLCJpYXQiOjB9.c2ln",
		identity.TokenPrefix + "looks-real-but-is-not",
	} {
		r := httptest.NewRequest("GET", "/api/v1/jobs", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		trusted, throttled := resolved(s, r)
		if trusted {
			t.Errorf("bearer %q was trusted to read anyone's jobs", bearer)
		}
		if !throttled {
			t.Errorf("bearer %q escaped the submit throttle", bearer)
		}
	}
}

// An untrusted caller with no session has nothing to scope to. Returning every
// job would leak other submitters' history, so the list must come back empty.
func TestListJobsWithoutSessionIsEmpty(t *testing.T) {
	h := openServer(t)
	r := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var got struct {
		Jobs   []any `json:"jobs"`
		Scoped bool  `json:"scoped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Jobs) != 0 {
		t.Fatalf("jobs = %d, want 0", len(got.Jobs))
	}
	if !got.Scoped {
		t.Fatal("an untrusted caller must be scoped")
	}
}

func TestClampInt(t *testing.T) {
	cases := []struct {
		raw            string
		def, lo, hi, w int
	}{
		{"", 50, 1, 500, 50},
		{"100", 50, 1, 500, 100},
		{"0", 50, 1, 500, 1},
		{"9999", 50, 1, 500, 500},
		{"abc", 50, 1, 500, 50},
		{"-5", 0, 0, 100, 0},
	}
	for _, tc := range cases {
		if got := clampInt(tc.raw, tc.def, tc.lo, tc.hi); got != tc.w {
			t.Fatalf("clampInt(%q) = %d, want %d", tc.raw, got, tc.w)
		}
	}
}

// docs/api.md documents dash-delimited variants ("chr17-7676154-C-T") but the
// engine splits a locus on ":", so the documented form fails the job with a
// confusing "bad locus" error. The handler translates; anything ambiguous is left
// alone so HGVS and rsIDs survive untouched.
func TestNormalizeLocus(t *testing.T) {
	cases := []struct{ in, want string }{
		{"chr17-7676154-C-T", "chr17:7676154:C:T"},
		{"chr17:7676154:C:T", "chr17:7676154:C:T"},       // already colon form
		{"chr1-100-ATTTT-A", "chr1:100:ATTTT:A"},         // deletion
		{"chr1-100-A-<DEL>", "chr1:100:A:<DEL>"},         // symbolic alt
		{"NM_000546.6:c.215C>G", "NM_000546.6:c.215C>G"}, // HGVS: has a colon, untouched
		{"rs28934578", "rs28934578"},                     // rsID: not four fields
		{"chr17-7676154-C", "chr17-7676154-C"},           // too few fields
		{"chr17-7676154-C-T-X", "chr17-7676154-C-T-X"},   // too many fields
		{"chr17-notapos-C-T", "chr17-notapos-C-T"},       // non-numeric position
		{"chr17--C-T", "chr17--C-T"},                     // empty field
	}
	for _, tc := range cases {
		if got := normalizeLocus(tc.in); got != tc.want {
			t.Fatalf("normalizeLocus(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
