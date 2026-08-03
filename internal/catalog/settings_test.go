package catalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Settings are the deployment's, and live apart from the manifest so re-fetching
// one from a registry cannot silently discard them.
func TestSourceSettingsSurviveAManifestReplacement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	put := func(toml string) {
		t.Helper()
		if err := s.PutSource(ctx, Source{
			ID: "gencode-49", Name: "gencode", Version: "49", Kind: "gtf",
			Build: "GRCh38", TOML: toml,
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("[[sources]]\nname=\"gencode\"\nversion=\"49\"\nannotation_prefix=\"GENCODE_49_\"\n")

	if err := s.PutSettings(ctx, "gencode-49", SourceSettings{
		AnnotationPrefix: "GENCODE_LATEST_", CacheSetup: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Re-registering the source replaces toml_text wholesale, which is what a
	// registry refresh does.
	put("[[sources]]\nname=\"gencode\"\nversion=\"49\"\nannotation_prefix=\"GENCODE_49_\"\n# refreshed\n")

	got, err := s.Settings(ctx, "gencode-49")
	if err != nil {
		t.Fatal(err)
	}
	if got.AnnotationPrefix != "GENCODE_LATEST_" || !got.CacheSetup {
		t.Errorf("settings lost on manifest replacement: %+v", got)
	}

	// Clearing everything removes the row, so "no settings" is one state.
	if err := s.PutSettings(ctx, "gencode-49", SourceSettings{}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Settings(ctx, "gencode-49"); !got.Empty() {
		t.Errorf("clearing left %+v", got)
	}
}

// The overlay is how a setting reaches varhub, so it has to appear in the job's
// materialized config.
func TestSettingsMaterializeIntoTheOverlay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "gencode-49", Name: "gencode", Version: "49", Kind: "gtf", Build: "GRCh38",
		TOML: "[[sources]]\nname=\"gencode\"\nversion=\"49\"\nassembly=\"GRCh38\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	m := &Materializer{
		Store: s, Root: t.TempDir(),
		DataDir: "/var/lib/varianthub/data", CacheDir: "/mnt/sources",
	}
	overlayPath := func(home string) string {
		return filepath.Join(home, "annotations", "sources", "gencode", "49",
			"gencode-49.locations.toml")
	}

	// Nothing set: no overlay, so resolution falls back to the convention
	// rather than reading a file that says nothing.
	home, cleanup, err := m.HomeForSources(ctx, []string{"gencode-49"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overlayPath(home)); !os.IsNotExist(err) {
		body, _ := os.ReadFile(overlayPath(home))
		t.Errorf("wrote an overlay with no settings:\n%s", body)
	}
	cleanup()

	if err := s.PutSettings(ctx, "gencode-49", SourceSettings{
		AnnotationPrefix: "GENCODE_LATEST_", CacheSetup: true,
	}); err != nil {
		t.Fatal(err)
	}
	home, cleanup, err = m.HomeForSources(ctx, []string{"gencode-49"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	body, err := os.ReadFile(overlayPath(home))
	if err != nil {
		t.Fatalf("no overlay written: %v", err)
	}
	for _, want := range []string{`annotation_prefix = "GENCODE_LATEST_"`, "cache_setup = true"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("overlay missing %q:\n%s", want, body)
		}
	}
}
