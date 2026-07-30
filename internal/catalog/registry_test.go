package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const manifestTOML = `
[[sources]]
name        = "clinvar"
version     = "2026-06"
assembly    = "GRCh38"
file        = "sources/clinvar/2026-06/clinvar-2026-06.toml"
latest      = true
description = "ClinVar clinical significance"

[[sources]]
name    = "clinvar"
version = "2026-01"
file    = "sources/clinvar/2026-01/clinvar-2026-01.toml"

[[snapshots]]
name = "2026-06"
file = "snapshots/2026-06.toml"
`

func registryServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.toml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(manifestTOML))
	})
	mux.HandleFunc("/sources/clinvar/2026-06/clinvar-2026-06.toml",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("[[sources]]\n  name = \"clinvar\"\n  version = \"2026-06\"\n"))
		})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestFetchManifest(t *testing.T) {
	srv := registryServer(t)
	m, err := FetchManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if len(m.Sources) != 2 || len(m.Snapshots) != 1 {
		t.Fatalf("got %d sources, %d snapshots", len(m.Sources), len(m.Snapshots))
	}
	if m.Sources[0].Ref() != "clinvar:2026-06" {
		t.Errorf("ref = %q", m.Sources[0].Ref())
	}
}

// A bare name resolves to the entry the publisher marked latest. Versions are not
// reliably sortable (semver 1.3, dbSNP b157, dates), so picking "the newest" by
// string order would silently choose wrong.
func TestFindEntryUsesLatestFlag(t *testing.T) {
	srv := registryServer(t)
	m, err := FetchManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"clinvar", "clinvar:latest"} {
		e, err := m.FindEntry(ref)
		if err != nil {
			t.Fatalf("FindEntry(%q): %v", ref, err)
		}
		if e.Version != "2026-06" {
			t.Errorf("FindEntry(%q) = %s, want the latest-flagged 2026-06", ref, e.Version)
		}
	}
	if e, err := m.FindEntry("clinvar:2026-01"); err != nil || e.Version != "2026-01" {
		t.Errorf("explicit version not honored: %+v %v", e, err)
	}
	if _, err := m.FindEntry("nope"); err == nil {
		t.Error("unknown ref should error")
	}
}

func TestFetchEntry(t *testing.T) {
	srv := registryServer(t)
	m, _ := FetchManifest(context.Background(), srv.URL)
	e, _ := m.FindEntry("clinvar")
	body, err := FetchEntry(context.Background(), srv.URL, e)
	if err != nil {
		t.Fatalf("FetchEntry: %v", err)
	}
	if !strings.Contains(body, `version = "2026-06"`) {
		t.Errorf("unexpected fragment:\n%s", body)
	}
}

// A registry supplies the file path. It must not be able to point the fetch at
// another host or above the registry's own directory.
func TestEntryURLRejectsEscapes(t *testing.T) {
	const base = "https://example.org/reg/registry.toml"
	for _, bad := range []string{
		"../../etc/passwd",
		"/absolute/path.toml",
		"https://evil.example/x.toml",
		"//evil.example/x.toml",
	} {
		if got, err := entryURL(base, bad); err == nil {
			t.Errorf("entryURL(%q) = %q, want an error", bad, got)
		}
	}
	got, err := entryURL(base, "sources/a/1/a-1.toml")
	if err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if got != "https://example.org/reg/sources/a/1/a-1.toml" {
		t.Errorf("entryURL = %q", got)
	}
}

func TestManifestURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://x.org/reg":               "https://x.org/reg/registry.toml",
		"https://x.org/reg/":              "https://x.org/reg/registry.toml",
		"https://x.org/reg/registry.toml": "https://x.org/reg/registry.toml",
	} {
		if got := manifestURL(in); got != want {
			t.Errorf("manifestURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateRegistryURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://x.org/r.toml", "file:///etc/passwd", "notaurl"} {
		if err := ValidateRegistryURL(bad); err == nil {
			t.Errorf("ValidateRegistryURL(%q) should fail", bad)
		}
	}
	if err := ValidateRegistryURL("https://x.org/registry.toml"); err != nil {
		t.Errorf("valid URL rejected: %v", err)
	}
}

func TestRegistryCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.SeedRegistry(ctx); err != nil {
		t.Fatal(err)
	}
	regs, err := s.ListRegistries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 || !regs[0].Builtin {
		t.Fatalf("seed = %+v, want one builtin registry", regs)
	}
	// Seeding twice must not duplicate.
	if err := s.SeedRegistry(ctx); err != nil {
		t.Fatal(err)
	}
	if regs, _ := s.ListRegistries(ctx); len(regs) != 1 {
		t.Errorf("re-seed added a duplicate: %d", len(regs))
	}

	if err := s.PutRegistry(ctx, Registry{
		ID: "lab", Name: "Lab registry", URL: "https://lab.example/registry.toml",
	}); err != nil {
		t.Fatal(err)
	}
	if regs, _ := s.ListRegistries(ctx); len(regs) != 2 || !regs[0].Builtin {
		t.Errorf("builtin should sort first: %+v", regs)
	}

	// The builtin is restored by the next seed, so deleting it would only look
	// like it worked.
	if err := s.DeleteRegistry(ctx, DefaultRegistryID); err == nil {
		t.Error("deleting the builtin registry should be refused")
	}
	if err := s.DeleteRegistry(ctx, "lab"); err != nil {
		t.Errorf("deleting a user registry: %v", err)
	}
	if err := s.PutRegistry(ctx, Registry{ID: "x", Name: "X", URL: "ftp://x"}); err == nil {
		t.Error("a bad URL should be refused")
	}
}
