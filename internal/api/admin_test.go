package api

import (
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

const fragment = `[[sources]]
  type    = "genelist"
  name    = "panel"
  version = "v2"
  title   = "Lab panel"
`

func TestSourceRequestDerive(t *testing.T) {
	src, err := sourceRequest{TOML: fragment}.derive()
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if src.ID != "panel-v2" || src.Name != "panel" || src.Version != "v2" {
		t.Errorf("derived %+v", src)
	}
	if src.Kind != "genelist" {
		t.Errorf("kind = %q, want genelist", src.Kind)
	}
	if src.Title != "Lab panel" {
		t.Errorf("title = %q", src.Title)
	}
	// Left empty when the request says nothing, so "not mentioned" stays distinct
	// from "make it closed" — the two callers want different things from silence.
	// Registration falls through to the store's closed default (asserted against a
	// real row by TestRegisteringWithoutSayingIsStillClosed); an update carries the
	// stored value forward, which is what stopped a manifest edit from silently
	// closing a public source.
	if src.Visibility != "" {
		t.Errorf("visibility = %q, want it left for the caller to default", src.Visibility)
	}
	// The manifest is stored byte-for-byte; varhub reads it, not our projection.
	if src.TOML != fragment {
		t.Errorf("TOML was rewritten")
	}
}

func TestSourceRequestOverrides(t *testing.T) {
	src, err := sourceRequest{
		TOML: fragment, ID: "custom", Title: "T", Detail: "D",
		Visibility: "private", Origin: "uploaded",
	}.derive()
	if err != nil {
		t.Fatal(err)
	}
	if src.ID != "custom" || src.Title != "T" || src.Detail != "D" ||
		src.Origin != "uploaded" || src.Visibility != catalog.VisibilityRestricted {
		t.Errorf("overrides not applied: %+v", src)
	}
}

func TestSourceRequestRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  sourceRequest
	}{
		{"empty toml", sourceRequest{TOML: "   "}},
		{"not toml", sourceRequest{TOML: "this is not = toml ["}},
		{"no sources entry", sourceRequest{TOML: `title = "x"`}},
		{"no name", sourceRequest{TOML: "[[sources]]\n  version = \"1\"\n"}},
		{"bad visibility", sourceRequest{TOML: fragment, Visibility: "secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.req.derive(); err == nil {
				t.Error("expected rejection")
			}
		})
	}
}

// A fragment declaring several sources must be refused, not silently reduced to
// its first entry — the others would vanish with no explanation.
func TestSourceRequestRejectsMultipleEntries(t *testing.T) {
	multi := fragment + `
[[sources]]
  name    = "other"
  version = "1"
`
	_, err := sourceRequest{TOML: multi}.derive()
	if err == nil {
		t.Fatal("expected rejection of a multi-source fragment")
	}
	if got := err.Error(); got == "" || !contains(got, "one source per file") {
		t.Errorf("error should explain the rule, got %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
