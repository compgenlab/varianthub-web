package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The flow the API explorer performs, end to end.
//
// The page mints a token with the signed-in session and then sends requests
// with that token — which is the whole point of it: the token makes the server
// treat the request as an external caller, so what the page shows is what a
// script gets, rather than a rehearsal of it.
func TestExplorerMintsATokenAndCallsTheAPIWithIt(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	u, _ := h.member(t, "explorer@example.com")
	sess := h.sessionFor(t, u.ID)

	// Minted through the session, exactly as the page does, and always for a
	// day: a token made to try something out should stop mattering on its own.
	w := h.doSession("POST", "/api/v1/auth/tokens", sess,
		map[string]any{"name": "API explorer", "days": 1})
	if w.Code != http.StatusCreated {
		t.Fatalf("mint = %d: %s", w.Code, w.Body.String())
	}
	var minted struct {
		Token struct {
			ExpiresAt int64 `json:"expires_at"`
			CreatedAt int64 `json:"created_at"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.Secret == "" {
		t.Fatal("no secret returned; the page would have nothing to send")
	}
	if got := minted.Token.ExpiresAt - minted.Token.CreatedAt; got != int64(24*time.Hour/time.Second) {
		t.Errorf("token lasts %ds, want one day", got)
	}

	// The document the page renders itself from.
	w = h.do("GET", "/api/v1/openapi.json", minted.Secret, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the explorer cannot fetch its own spec: %d %s", w.Code, w.Body.String())
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}

	// Every endpoint the page offers must actually answer that token. A path
	// listed but unreachable is the page showing something that cannot work.
	for path, ops := range doc.Paths {
		for method := range ops {
			if method != "get" {
				continue // the rest need bodies or ids; covered elsewhere
			}
			probe := path
			if strings.Contains(probe, "{") {
				// Templated: probing it would send a literal "{id}" and get a
				// 404 about the missing thing rather than about the route.
				continue
			}
			got := h.do("GET", probe, minted.Secret, nil)
			if got.Code == http.StatusNotFound {
				t.Errorf("%s is offered by the document but 404s for a token: %s",
					probe, got.Body.String())
			}
			if got.Code == http.StatusUnauthorized {
				t.Errorf("%s rejected a freshly minted token", probe)
			}
		}
	}

	// And the surface split still holds for the page's own token: the endpoints
	// it does not list are the endpoints it cannot reach, which is why there is
	// no confusing 404 to explain to anyone.
	for _, hidden := range []string{"/api/v1/auth/tokens", "/api/v1/admin/storage"} {
		if got := h.do("GET", hidden, minted.Secret, nil); got.Code != http.StatusNotFound {
			t.Errorf("%s = %d with the explorer's token, want 404", hidden, got.Code)
		}
	}
}

// A token the explorer minted authenticates a real annotation submission, which
// is the request people will actually try first.
func TestExplorerTokenCanSubmitWork(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	u, _ := h.member(t, "explorer2@example.com")
	sess := h.sessionFor(t, u.ID)

	w := h.doSession("POST", "/api/v1/auth/tokens", sess,
		map[string]any{"name": "API explorer", "days": 1})
	var minted struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}

	// No catalog in this harness, so the submission cannot resolve a snapshot —
	// but it must fail on that, not on the credential. 401 here would mean the
	// explorer's token does not carry submission rights at all.
	got := h.do("POST", "/api/v1/annotate", minted.Secret, map[string]any{
		"snapshot": "nope", "variants": []string{"chr12:25245350:C:T"},
	})
	if got.Code == http.StatusUnauthorized || got.Code == http.StatusNotFound {
		t.Errorf("submitting with an explorer token = %d (%s); it should be "+
			"authenticated and fail only on the snapshot", got.Code, got.Body.String())
	}
}
