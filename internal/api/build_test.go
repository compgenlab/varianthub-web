package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// Adding or removing a build is administration: it changes what every user is
// offered when starting an annotation. Driven through the real router with real
// tokens, so routing, requireAdmin and the handler are all covered.
func TestBuildRoutesRequireAnAdmin(t *testing.T) {
	h := newHarness(t)
	admin, adminTok := h.admin(t)
	member, _ := h.member(t, "member@example.com")
	adminSess := h.sessionFor(t, admin.ID)
	memberSess := h.sessionFor(t, member.ID)

	cases := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/v1/admin/builds", map[string]any{"name": "GRCm39"}},
		{"DELETE", "/api/v1/admin/builds/GRCm39", nil},
	}
	for _, tc := range cases {
		if w := h.doSession(tc.method, tc.path, "", tc.body); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", tc.method, tc.path, w.Code)
		}
		if w := h.doSession(tc.method, tc.path, memberSess, tc.body); w.Code != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403", tc.method, tc.path, w.Code)
		}
		// Administration is not published API, so a token does not reach it even
		// when its owner is an administrator — which is a different answer from
		// the member's 403, and deliberately so.
		if w := h.do(tc.method, tc.path, adminTok, tc.body); w.Code != http.StatusNotFound {
			t.Errorf("admin token %s %s = %d, want 404", tc.method, tc.path, w.Code)
		}
	}

	// The administrator's round trip, in order: create, then remove.
	if w := h.doSession("PUT", "/api/v1/admin/builds", adminSess,
		map[string]any{"name": "GRCm39", "label": "Mouse", "sort_order": 2}); w.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d, body %s", w.Code, w.Body.String())
	}
	if w := h.doSession("DELETE", "/api/v1/admin/builds/GRCm39", adminSess, nil); w.Code != http.StatusNoContent {
		t.Fatalf("admin DELETE = %d, body %s", w.Code, w.Body.String())
	}
}

// The annotation form needs the build list to populate its picker and to filter
// the source list, so the gate is requireAuth — the same as /sources — and not
// requireAdmin. A member who cannot read it cannot start an annotation.
func TestListBuildsIsReadableByAnyUser(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.admin(t)
	_, memberTok := h.member(t, "member@example.com")
	adminSess := h.sessionFor(t, admin.ID)
	ctx := context.Background()

	if err := h.cat.PutBuild(ctx, catalog.Build{Name: "GRCh38", Label: "Human"}); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.PutSource(ctx, catalog.Source{
		ID: "g", Name: "g", Version: "1", Kind: "gtf", Build: "GRCh38", TOML: "[[sources]]\n",
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do("GET", "/api/v1/builds", memberTok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("member GET /api/v1/builds = %d, body %s", w.Code, w.Body.String())
	}
	var got struct {
		Builds []catalog.Build `json:"builds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Builds) != 1 || got.Builds[0].Name != "GRCh38" || got.Builds[0].Label != "Human" {
		t.Fatalf("builds = %+v", got.Builds)
	}
	// The count is what tells an administrator what removing it would strand.
	if got.Builds[0].Sources != 1 {
		t.Errorf("GRCh38 reported %d sources, want 1", got.Builds[0].Sources)
	}

	// Removing it is refused with 409, not 400: the request is well-formed and
	// becomes valid once the sources move, which is a different thing to tell a
	// client than "you sent nonsense".
	if w := h.doSession("DELETE", "/api/v1/admin/builds/GRCh38", adminSess, nil); w.Code != http.StatusConflict {
		t.Fatalf("DELETE of a build in use = %d, want 409; body %s", w.Code, w.Body.String())
	}
}
