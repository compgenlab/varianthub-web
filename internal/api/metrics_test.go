package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The point of the fallback is origins that refuse HEAD; a size has to come back
// from a ranged GET in that case, because that is the request the annotator
// makes anyway.
func TestRemoteSizeProbe(t *testing.T) {
	const size = 4096
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    int64
		wantOK  bool
	}{
		{
			name: "HEAD reports the length",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("fell back to %s when HEAD worked", r.Method)
				}
				w.Header().Set("Content-Length", fmt.Sprint(size))
				w.WriteHeader(http.StatusOK)
			},
			want: size, wantOK: true,
		},
		{
			name: "HEAD refused, range answers",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size))
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte{0})
			},
			want: size, wantOK: true,
		},
		{
			// An origin that knows the range but not the total answers "*". A
			// number must not be invented for it.
			name: "unknown total",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Range", "bytes 0-0/*")
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte{0})
			},
			wantOK: false,
		},
		{
			name: "origin is down",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			rs := newRemoteSizer()
			got, ok := rs.size(context.Background(), srv.URL+"/data.vcf.gz")
			if ok != tc.wantOK {
				t.Fatalf("measured = %v, want %v (size %d)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("size = %d, want %d", got, tc.want)
			}
		})
	}
}

// A dashboard load must not re-probe somebody else's server for every file it
// already knows the size of.
func TestRemoteSizeIsCached(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rs := newRemoteSizer()
	for i := 0; i < 3; i++ {
		if _, ok := rs.size(context.Background(), srv.URL+"/f.gz"); !ok {
			t.Fatal("probe failed")
		}
	}
	if hits != 1 {
		t.Errorf("origin was hit %d times, want 1", hits)
	}
}

// Totals report only what was measured, and say so when something wasn't —
// a floor stated as a floor beats a wrong number stated confidently.
func TestSizesReportsUnmeasured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good.gz" {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rs := newRemoteSizer()
	total, unmeasured := rs.sizes(context.Background(),
		[]string{srv.URL + "/good.gz", srv.URL + "/missing.gz"})
	if total != 1000 {
		t.Errorf("total = %d, want 1000 (the measurable file only)", total)
	}
	if unmeasured != 1 {
		t.Errorf("unmeasured = %d, want 1", unmeasured)
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://storage.googleapis.com/gcp-public-data/x.vcf.gz": "storage.googleapis.com",
		"http://localhost:8299/clinvar.vcf.gz":                    "localhost:8299",
		"s3://bucket/prefix/x.gz":                                 "bucket",
		"":                                                        "",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
