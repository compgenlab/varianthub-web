package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/auth"
	"github.com/compgenlab/varianthub-web/internal/config"
)

func testServer(t *testing.T, requireToken bool) *Server {
	t.Helper()
	return New(&config.Config{
		MasterKey:    "test-key",
		RequireToken: requireToken,
		Version:      "test",
		RatePerMin:   1000,
		RateBurst:    1000,
	}, nil, nil, nil)
}

func TestVersionIsOpen(t *testing.T) {
	h := testServer(t, true).Routes()
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

func TestV1RequiresToken(t *testing.T) {
	h := testServer(t, true).Routes()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/ping", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless /api/v1/ping = %d, want 401", w.Code)
	}

	tok, err := auth.MintToken("test-key", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v1/ping", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated /api/v1/ping = %d, want 200", w.Code)
	}
}

func TestRequireTokenFalseOpensV1(t *testing.T) {
	h := testServer(t, false).Routes()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("open /api/v1/ping = %d, want 200", w.Code)
	}
}

// A wrong key must not authenticate, even though the token is well-formed.
func TestWrongKeyRejected(t *testing.T) {
	tok, err := auth.MintToken("other-key", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v1/ping", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	testServer(t, true).Routes().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("foreign-key token = %d, want 401", w.Code)
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
		MasterKey: "k", RequireToken: true, Version: "test",
		CORSOrigins: []string{"https://app.example"},
	}, nil, nil, nil)
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
