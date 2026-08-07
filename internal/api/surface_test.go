package api

import (
	"net/http"
	"strings"
	"testing"
)

// The published REST API is a curated subset. A token is what a program outside
// the web app holds, so it decides which surface answers — not what the caller
// is allowed to do, which is unchanged.
func TestPublishedSurfaceIsReachableWithAToken(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)       // /jobs needs a real queue
	_, tok := h.admin(t) // an admin token: proves the gate is not about rights

	// Every route a program driving annotations needs. None may 404.
	for _, path := range []string{
		"/api/v1/ping",
		"/api/v1/builds",
		"/api/v1/sources",
		"/api/v1/snapshots",
		"/api/v1/jobs",
	} {
		if w := h.do("GET", path, tok, nil); w.Code == http.StatusNotFound {
			t.Errorf("GET %s is not reachable with a token: %s", path, w.Body.String())
		}
	}
}

// Web-app surface: paginated tables, session and token management, and the whole
// administration area. Hidden from a token even when its owner is an admin,
// because the point is how much API is published, not who may use it.
func TestWebOnlySurfaceIsHiddenFromAToken(t *testing.T) {
	h := newHarness(t)
	_, tok := h.admin(t)

	cases := []struct{ method, path string }{
		{"GET", "/api/v1/auth/me"},
		{"GET", "/api/v1/auth/tokens"},
		{"POST", "/api/v1/auth/tokens"},
		{"POST", "/api/v1/auth/login"},
		{"GET", "/api/v1/jobs/abc/results"},
		{"GET", "/api/v1/jobs/abc/log"},
		{"GET", "/api/v1/admin/storage"},
		{"GET", "/api/v1/admin/users"},
		{"PUT", "/api/v1/admin/builds"},
		{"GET", "/api/v1/admin/registries"},
	}
	for _, tc := range cases {
		w := h.do(tc.method, tc.path, tok, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d with a token, want 404", tc.method, tc.path, w.Code)
		}
		// 404 rather than 403 on purpose: 403 advertises an endpoint and invites
		// hunting for the permission that opens it, which no token has.
		if w.Code == http.StatusForbidden {
			t.Errorf("%s %s returned 403, which advertises the route", tc.method, tc.path)
		}
	}
}

// The same routes must keep working for the browser, which is the whole reason
// they exist. A session cookie is not a token.
func TestWebOnlySurfaceStillAnswersASession(t *testing.T) {
	h := newHarness(t)
	sess := h.session(t)

	for _, path := range []string{
		"/api/v1/auth/me",
		"/api/v1/auth/tokens",
		"/api/v1/admin/storage",
		"/api/v1/admin/users",
	} {
		w := h.doSession("GET", path, sess, nil)
		if w.Code == http.StatusNotFound {
			t.Errorf("GET %s = 404 for a signed-in session; the web app needs it: %s",
				path, w.Body.String())
		}
	}
}

// Export is the published way to get results: already the whole matching set in
// a chosen format, so it serves the browser's download and the REST API alike.
func TestExportIsPublishedAndResultsIsNot(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	if w := h.do("GET", "/api/v1/jobs/abc/export", tok, nil); strings.Contains(
		w.Body.String(), "web application") {
		t.Errorf("export is hidden from the REST API: %s", w.Body.String())
	}
	if w := h.do("GET", "/api/v1/jobs/abc/results", tok, nil); w.Code != http.StatusNotFound {
		t.Errorf("results = %d with a token, want 404", w.Code)
	}
}
