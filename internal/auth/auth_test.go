package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMintVerifyRoundTrip(t *testing.T) {
	const key = "s3cr3t-master-key"
	tok, err := MintToken(key, 1_700_000_000)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if !VerifyToken(key, tok) {
		t.Fatalf("VerifyToken rejected a freshly minted token")
	}
}

func TestVerifyRejectsTamperAndWrongKey(t *testing.T) {
	const key = "correct-key"
	tok, _ := MintToken(key, 0)

	if VerifyToken("other-key", tok) {
		t.Errorf("token verified under the wrong master key")
	}
	// Tamper with the signature.
	if VerifyToken(key, tok+"x") {
		t.Errorf("token verified after signature was appended to")
	}
	// Tamper with the payload (flip a char in the body).
	body := []byte(tok)
	for i := 0; i < len(body) && body[i] != '.'; i++ {
		body[i] ^= body[i] // zero it — still base64-ish but not the signed body
	}
	if VerifyToken(key, string(body)) {
		t.Errorf("token verified after payload was altered")
	}
	for _, bad := range []string{"", "no-dot", ".", "a.", ".b"} {
		if VerifyToken(key, bad) {
			t.Errorf("malformed token %q verified", bad)
		}
	}
}

func TestRequireTokenMiddleware(t *testing.T) {
	const key = "k"
	tok, _ := MintToken(key, 0)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireToken(key, inner)

	cases := []struct {
		name, header string
		want         int
	}{
		{"valid", "Bearer " + tok, http.StatusOK},
		{"missing", "", http.StatusUnauthorized},
		{"not-bearer", tok, http.StatusUnauthorized},
		{"bad-token", "Bearer nope.nope", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/annotations", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}
