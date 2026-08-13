package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Reading and cancelling are different permissions, and they differ exactly
// where an anonymous job is concerned:
//
//	                view                     cancel
//	signed-in       the owning account       the owning account
//	anonymous       anyone with the link     the submitting session
//
// The asymmetry is the point. A link is shared so someone can look at a result;
// that should not also let them stop the run producing it.
func TestWhoMayViewAndWhoMayCancel(t *testing.T) {
	s := &Server{}

	t.Run("anonymous job is readable by a stranger", func(t *testing.T) {
		job := queue.Chunk{ID: "abc", UserID: "", Session: "submitters-session"}
		r := httptest.NewRequest("GET", "/api/v1/jobs/abc", nil)
		if !s.canView(r, job) {
			t.Error("a shared anonymous link could not be read")
		}
	})

	t.Run("anonymous job is not cancellable by a stranger", func(t *testing.T) {
		job := queue.Chunk{ID: "abc", UserID: "", Session: "submitters-session"}
		r := httptest.NewRequest("POST", "/api/v1/jobs/abc/cancel", nil)
		if s.owns(r, job) {
			t.Error("holding the link was enough to cancel someone else's run")
		}
	})

	t.Run("signed-in job is private to its account", func(t *testing.T) {
		job := queue.Chunk{ID: "abc", UserID: "user-1"}
		r := httptest.NewRequest("GET", "/api/v1/jobs/abc", nil)
		if s.canView(r, job) {
			t.Error("a signed-in user's result was readable by an unauthenticated caller")
		}
	})

	t.Run("an unset session does not own an unset session", func(t *testing.T) {
		// The case that would quietly undo the split: a caller with no session
		// matching a job whose session is also empty would make every anonymous
		// job cancellable by anybody.
		job := queue.Chunk{ID: "abc", UserID: "", Session: ""}
		r := httptest.NewRequest("POST", "/api/v1/jobs/abc/cancel", nil)
		if s.owns(r, job) {
			t.Error("empty session matched empty session; anonymous jobs are cancellable by anyone")
		}
	})
}

// A shared link has to work for a client that carries nothing at all.
//
// The link IS the credential for an anonymous result, so requiring a second one
// defeats the sharing it exists for. It worked in a browser only because the
// app hands every visitor a session on page load; the same URL given to curl
// was refused before anyone asked whose job it was.
func TestAnonymousResultIsReadableWithNoCredentialAtAll(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.AllowAnonymous = true
	h.withQueue(t)

	sess := h.anon(t)
	r := httptest.NewRequest("POST", "/api/v1/annotate",
		strings.NewReader(`{"snapshot":"s","variants":["chr1:100:A:G"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: AnonCookie, Value: sess})
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted && w.Code != http.StatusOK {
		t.Fatalf("submit = %d (%s)", w.Code, w.Body.String())
	}
	var body struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	// No cookie, no bearer, nothing — curl with a URL.
	bare := func(path string) int {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.http.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := bare("/api/v1/jobs/" + body.JobID); got != http.StatusOK {
		t.Errorf("bare read of an anonymous job = %d, want 200", got)
	}
	// export is the published way to get results, so it has to work too.
	if got := bare("/api/v1/jobs/" + body.JobID + "/export"); got == http.StatusUnauthorized {
		t.Errorf("bare export was refused for want of a credential (%d)", got)
	}

	// And the hole this must not open: an id that does not exist still 404s
	// rather than revealing anything, and a credential-less caller gains
	// nothing beyond the single id it was given.
	if got := bare("/api/v1/jobs/does-not-exist"); got != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", got)
	}
	if got := bare("/api/v1/jobs"); got != http.StatusUnauthorized {
		t.Errorf("listing jobs with no credential = %d, want 401 — a link is one "+
			"result, not an account", got)
	}
}
