package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/config"
)

// testServer has no identity store, so no credential can resolve: every caller
// is anonymous, which is exactly the state these tests are about.
func testServer(t *testing.T, allowAnonymous bool) *Server {
	t.Helper()
	return New(&config.Config{
		AllowAnonymous: allowAnonymous,
		Version:        "test",
		RatePerMin:     1000,
		RateBurst:      1000,
	}, nil, nil, nil, nil)
}

func TestVersionIsOpen(t *testing.T) {
	h := testServer(t, false).Routes()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/version", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/version = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != "test" {
		t.Errorf("version = %q", got["version"])
	}
}

func TestV1RequiresAnIdentity(t *testing.T) {
	h := testServer(t, false).Routes()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/ping", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/v1/ping = %d, want 401", w.Code)
	}
}

func TestAllowAnonymousOpensV1(t *testing.T) {
	h := testServer(t, true).Routes()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("open /api/v1/ping = %d, want 200", w.Code)
	}
}

// There is no deployment-wide key any more. A well-formed-looking bearer value
// that is not one of our credentials resolves to anonymous — it must not be
// mistaken for a shared secret that once worked here.
func TestUnknownBearerIsAnonymous(t *testing.T) {
	for _, bearer := range []string{
		"eyJzdWIiOiJ2YXJodWIiLCJpYXQiOjB9.c2ln", // the shape the old master key had
		"cgl_vh_notarealtoken",
		"Basic dXNlcjpwdw==",
	} {
		r := httptest.NewRequest("GET", "/api/v1/ping", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		testServer(t, false).Routes().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q = %d, want 401", bearer, w.Code)
		}
	}
}

// CORS headers must be absent unless origins are configured — the default is a
// same-origin deployment, where advertising CORS would only widen the surface.
func TestCORSOffByDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/version", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	testServer(t, true).Routes().ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unexpected CORS header %q with no configured origins", got)
	}
}

func TestCORSAllowsConfiguredOrigin(t *testing.T) {
	s := New(&config.Config{
		Version:     "test",
		CORSOrigins: []string{"https://app.example"},
	}, nil, nil, nil, nil)
	h := s.Routes()

	r := httptest.NewRequest("GET", "/version", nil)
	r.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("Allow-Origin = %q, want the configured origin", got)
	}

	// An origin that is not on the list gets no header.
	r2 := httptest.NewRequest("GET", "/version", nil)
	r2.Header.Set("Origin", "https://evil.example")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q for an unlisted origin", got)
	}
}
