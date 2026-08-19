package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// seedSnapshot publishes a snapshot to submit against.
func (h *harness) seedSnapshot(t *testing.T) {
	t.Helper()
	if err := h.cat.PutSnapshot(context.Background(), catalog.Snapshot{
		ID: "snap", Build: "GRCh38", State: catalog.StatePublished,
	}, nil); err != nil {
		t.Fatal(err)
	}
}

// Who may ask this service to make an HTTP request, and to where.

func callbackJobURL(t *testing.T, h *harness, jobID string) string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var url *string
	if err := pool.QueryRow(context.Background(),
		`SELECT callback_url FROM job WHERE id=$1`, jobID).Scan(&url); err != nil {
		t.Fatalf("read callback_url: %v", err)
	}
	if url == nil {
		return ""
	}
	return *url
}

func TestASubmittedCallbackIsStoredOnTheJob(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	h.seedSnapshot(t)
	_, tok := h.admin(t)

	var out struct {
		JobID string `json:"job_id"`
	}
	w := h.do("POST", "/api/v1/annotate", tok, map[string]any{
		"snapshot": "snap", "variants": []string{"chr1:100:A:T"},
		"callback_url": "https://hooks.example.org/vh",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit = %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := callbackJobURL(t, h, out.JobID); got != "https://hooks.example.org/vh" {
		t.Errorf("stored callback_url = %q", got)
	}
}

// Plain http is refused: a notification is unsigned and unencrypted, so over
// http its contents are readable by anything on the path — and there is no
// signature to tell the receiver it came from us either.
func TestAPlainHTTPCallbackIsRefused(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	h.seedSnapshot(t)
	_, tok := h.admin(t)

	for _, bad := range []string{
		"http://hooks.example.org/vh",
		"ftp://hooks.example.org/vh",
		"https://user:pass@hooks.example.org/vh",
		"not a url",
	} {
		w := h.do("POST", "/api/v1/annotate", tok, map[string]any{
			"snapshot": "snap", "variants": []string{"chr1:100:A:T"},
			"callback_url": bad,
		})
		if w.Code != http.StatusBadRequest {
			t.Errorf("callback_url=%q gave %d, want 400: %s", bad, w.Code, w.Body.String())
		}
	}
}

// An anonymous visitor is a session that outlives nothing. Letting one make
// this service issue requests to an address of their choosing is the SSRF
// surface with the accountability removed — the address rules still apply, but
// nobody is left to ask about it afterwards.
func TestAnAnonymousCallerCannotSetACallback(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.AllowAnonymous = true
	h.withQueue(t)
	h.seedSnapshot(t)
	sess := h.anon(t)

	w := h.doAnon("POST", "/api/v1/annotate", sess, map[string]any{
		"snapshot": "snap", "variants": []string{"chr1:100:A:T"},
		"callback_url": "https://hooks.example.org/vh",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("anonymous callback = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "account") {
		t.Errorf("the refusal does not say what is missing: %s", w.Body.String())
	}

	// And the same submission without one still works, so this restricts the
	// callback rather than the caller.
	w = h.doAnon("POST", "/api/v1/annotate", sess, map[string]any{
		"snapshot": "snap", "variants": []string{"chr1:100:A:T"},
	})
	if w.Code != http.StatusAccepted {
		t.Errorf("anonymous submission without a callback = %d: %s", w.Code, w.Body.String())
	}
}
