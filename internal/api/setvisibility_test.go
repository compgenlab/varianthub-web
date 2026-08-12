package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

const visSourceTOML = `[[sources]]
  format   = "vcf"
  name     = "clinvar"
  version  = "2026"
  assembly = "GRCh38"
  url      = "https://example.invalid/clinvar.vcf.gz"

  [[sources.annotations]]
    name  = "clinvar_sig"
    field = "CLNSIG"
`

func visHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	if err := h.cat.PutSource(context.Background(), catalog.Source{
		ID: "clinvar-2026", Name: "clinvar", Version: "2026", Kind: "vcf", Build: "GRCh38",
		Visibility: catalog.VisibilityPublic, TOML: visSourceTOML,
	}); err != nil {
		t.Fatal(err)
	}
	return h, h.session(t)
}

func sourceVisibility(t *testing.T, h *harness, id string) string {
	t.Helper()
	src, err := h.cat.GetSource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return src.Visibility
}

func TestSetSourceVisibilityCyclesThroughAllThreeLevels(t *testing.T) {
	h, sess := visHarness(t)

	for _, level := range []string{
		catalog.VisibilitySignedIn,
		catalog.VisibilityRestricted,
		catalog.VisibilityPublic,
	} {
		got := h.doSession("PUT", "/api/v1/admin/sources/clinvar-2026/visibility", sess,
			visibilityRequest{Visibility: level})
		if got.Code != http.StatusOK {
			t.Fatalf("set %s = %d: %s", level, got.Code, got.Body)
		}
		if v := sourceVisibility(t, h, "clinvar-2026"); v != level {
			t.Errorf("after setting %s the source is %s", level, v)
		}
	}
}

// A source a snapshot pins cannot have its manifest rewritten — that is a promise
// about what an annotation ran against. Closing it off is a different thing, and
// must not be blocked by the same rule.
func TestVisibilityCanBeChangedOnAPinnedSource(t *testing.T) {
	h, sess := visHarness(t)
	ctx := context.Background()
	if err := h.cat.PutSnapshot(ctx,
		catalog.Snapshot{ID: "snap", Build: "GRCh38"}, []string{"clinvar-2026"}); err != nil {
		t.Fatal(err)
	}

	got := h.doSession("PUT", "/api/v1/admin/sources/clinvar-2026/visibility", sess,
		visibilityRequest{Visibility: catalog.VisibilityRestricted})
	if got.Code != http.StatusOK {
		t.Fatalf("a pinned source could not be closed off: %d %s", got.Code, got.Body)
	}
	if v := sourceVisibility(t, h, "clinvar-2026"); v != catalog.VisibilityRestricted {
		t.Errorf("visibility = %s", v)
	}
}

func TestSetSourceVisibilityRejectsNonsense(t *testing.T) {
	h, sess := visHarness(t)
	for _, bad := range []string{"", "everyone", "PUBLIC-ish"} {
		got := h.doSession("PUT", "/api/v1/admin/sources/clinvar-2026/visibility", sess,
			visibilityRequest{Visibility: bad})
		if got.Code != http.StatusBadRequest {
			t.Errorf("visibility %q = %d, want 400", bad, got.Code)
		}
	}
	// And the source is untouched.
	if v := sourceVisibility(t, h, "clinvar-2026"); v != catalog.VisibilityPublic {
		t.Errorf("a rejected request changed the source to %s", v)
	}
	got := h.doSession("PUT", "/api/v1/admin/sources/nope/visibility", sess,
		visibilityRequest{Visibility: catalog.VisibilityPublic})
	if got.Code != http.StatusNotFound {
		t.Errorf("unknown source = %d, want 404", got.Code)
	}
}

// The regression. The manifest editor posts only the TOML, and the default used
// to land on the row — so saving an unrelated one-line change took a public
// source away from every anonymous caller using it, with nothing to say so.
func TestEditingAManifestDoesNotChangeWhoCanSeeIt(t *testing.T) {
	h, sess := visHarness(t)

	for _, level := range []string{
		catalog.VisibilityPublic,
		catalog.VisibilitySignedIn,
		catalog.VisibilityRestricted,
	} {
		if _, err := h.cat.SetSourceVisibility(context.Background(), "clinvar-2026", level); err != nil {
			t.Fatal(err)
		}
		// Exactly what the UI sends: the manifest and nothing else.
		edited := visSourceTOML + "\n  [[sources.annotations]]\n    name = \"clinvar_id\"\n    field = \"ALLELEID\"\n"
		got := h.doSession("PUT", "/api/v1/admin/sources/clinvar-2026", sess,
			map[string]string{"toml": edited})
		if got.Code != http.StatusOK {
			t.Fatalf("edit at %s = %d: %s", level, got.Code, got.Body)
		}
		if v := sourceVisibility(t, h, "clinvar-2026"); v != level {
			t.Errorf("editing the manifest changed a %s source to %s", level, v)
		}
	}
}

// Registration is the other side of that: silence there still means closed.
func TestRegisteringWithoutSayingIsStillClosed(t *testing.T) {
	h, sess := visHarness(t)

	got := h.doSession("POST", "/api/v1/admin/sources", sess, map[string]string{
		"toml": strings.Replace(visSourceTOML, `version  = "2026"`, `version  = "2027"`, 1),
	})
	if got.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", got.Code, got.Body)
	}
	if v := sourceVisibility(t, h, "clinvar-2027"); v != catalog.VisibilityRestricted {
		t.Errorf("a source registered with no visibility is %s, want restricted", v)
	}
}

func TestSetSnapshotVisibility(t *testing.T) {
	h, sess := visHarness(t)
	ctx := context.Background()
	if err := h.cat.PutSnapshot(ctx,
		catalog.Snapshot{ID: "snap", Build: "GRCh38"}, []string{"clinvar-2026"}); err != nil {
		t.Fatal(err)
	}

	got := h.doSession("PUT", "/api/v1/admin/snapshots/snap/visibility", sess,
		visibilityRequest{Visibility: catalog.VisibilitySignedIn})
	if got.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", got.Code, got.Body)
	}
	var body struct {
		Visibility    string   `json:"visibility"`
		Effective     string   `json:"effective_visibility"`
		ConstrainedBy []string `json:"constrained_by"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Its sources are public, so its own level is the binding one.
	if body.Visibility != catalog.VisibilitySignedIn || body.Effective != catalog.VisibilitySignedIn {
		t.Errorf("stored %q effective %q", body.Visibility, body.Effective)
	}
	if len(body.ConstrainedBy) != 0 {
		t.Errorf("nothing constrains this snapshot, but it reported %v", body.ConstrainedBy)
	}
}

// Setting a snapshot to public when it pins something that is not has to say so.
// Otherwise it looks like the setting was ignored.
func TestASnapshotCannotBeOpenedPastItsSources(t *testing.T) {
	h, sess := visHarness(t)
	ctx := context.Background()
	if _, err := h.cat.SetSourceVisibility(ctx, "clinvar-2026", catalog.VisibilityRestricted); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.PutSnapshot(ctx,
		catalog.Snapshot{ID: "snap", Build: "GRCh38"}, []string{"clinvar-2026"}); err != nil {
		t.Fatal(err)
	}

	got := h.doSession("PUT", "/api/v1/admin/snapshots/snap/visibility", sess,
		visibilityRequest{Visibility: catalog.VisibilityPublic})
	if got.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", got.Code, got.Body)
	}
	var body struct {
		Visibility    string   `json:"visibility"`
		Effective     string   `json:"effective_visibility"`
		ConstrainedBy []string `json:"constrained_by"`
		Note          string   `json:"note"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Effective != catalog.VisibilityRestricted {
		t.Errorf("effective = %q, want restricted — the pinned source decides", body.Effective)
	}
	if len(body.ConstrainedBy) != 1 || !strings.Contains(body.ConstrainedBy[0], "clinvar:2026") {
		t.Errorf("constrained_by = %v, want the source that is doing it", body.ConstrainedBy)
	}
	if body.Note == "" {
		t.Error("no note explaining why the stored level is not the effective one")
	}
}

// The toggles are the web app's surface, not the published API's — the same split
// every other admin route follows.
func TestVisibilityTogglesAreNotOnTheTokenSurface(t *testing.T) {
	h, _ := visHarness(t)
	_, tok := h.admin(t)

	for _, path := range []string{
		"/api/v1/admin/sources/clinvar-2026/visibility",
		"/api/v1/admin/snapshots/snap/visibility",
	} {
		got := h.do("PUT", path, tok, visibilityRequest{Visibility: catalog.VisibilityPublic})
		if got.Code != http.StatusNotFound {
			t.Errorf("PUT %s with a token = %d, want 404", path, got.Code)
		}
	}
}

// And a member cannot change who may see things.
func TestAMemberCannotChangeVisibility(t *testing.T) {
	h, _ := visHarness(t)
	u, _ := h.member(t, "member@example.com")
	sess := h.sessionFor(t, u.ID)

	got := h.doSession("PUT", "/api/v1/admin/sources/clinvar-2026/visibility", sess,
		visibilityRequest{Visibility: catalog.VisibilityPublic})
	if got.Code == http.StatusOK {
		t.Error("a member changed a source's visibility")
	}
	if v := sourceVisibility(t, h, "clinvar-2026"); v != catalog.VisibilityPublic {
		t.Errorf("visibility changed to %s", v)
	}
}
