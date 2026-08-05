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

// The names the API advertises must be the names annotation actually emits.
//
// This is how the prefix feature broke in the dev stack: builtins carried an
// override of "CG_", the field picker listed the manifest's bare "auto_id", the
// snapshot pinned that as a default, and materialization emitted "CG_auto_id" —
// so every job against that snapshot died with `default_annotations references
// unknown annotation "auto_id"`, reported by varhub at annotate time with
// nothing pointing back at the listing that invented the name.
func TestListedNamesCarryTheDeploymentPrefix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// builtins declares no prefix of its own — the case that failed.
	if err := s.PutSource(ctx, Source{
		ID: "builtins", Name: "builtins", Version: "1", Kind: "builtin",
		TOML: "[[sources]]\ntype=\"builtin\"\nname=\"builtins\"\nversion=\"1\"\n" +
			"[[sources.annotations]]\nbuiltin=\"auto_id\"\nname=\"auto_id\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	// VEP declares one, so an override must replace it rather than stack.
	if err := s.PutSource(ctx, Source{
		ID: "vep-113", Name: "vep", Version: "113", Kind: "tool", Build: "GRCh38",
		TOML: "[[sources]]\nname=\"vep\"\nversion=\"113\"\nannotation_prefix=\"VEP_\"\n" +
			"[[sources.annotations]]\nname=\"VEP_Allele\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	for id, prefix := range map[string]string{"builtins": "CG_", "vep-113": "VEP_113_"} {
		if err := s.PutSettings(ctx, id, SourceSettings{AnnotationPrefix: prefix}); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]string{"builtins": "CG_auto_id", "vep-113": "VEP_113_Allele"}
	for id, wantName := range want {
		src, err := s.GetSource(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		anns := src.Annotations()
		if len(anns) != 1 {
			t.Fatalf("%s: got %d annotations, want 1", id, len(anns))
		}
		if anns[0].Name != wantName {
			t.Errorf("%s: listed as %q, but annotation will emit %q",
				id, anns[0].Name, wantName)
		}
	}

	// Renaming must not change what is read from the file, only what is written.
	src, _ := s.GetSource(ctx, "vep-113")
	if f := src.Annotations()[0].Field; f != "VEP_Allele" {
		t.Errorf("Field = %q, want the source's own VEP_Allele; renaming the "+
			"output must not change what is looked up", f)
	}
	// A builtin computes its value and has no field to read.
	src, _ = s.GetSource(ctx, "builtins")
	if f := src.Annotations()[0].Field; f != "" {
		t.Errorf("builtin got Field %q; it reads no column", f)
	}

	// "-" is how a deployment says "no prefix at all", which "" cannot express.
	if err := s.PutSettings(ctx, "vep-113", SourceSettings{AnnotationPrefix: "-"}); err != nil {
		t.Fatal(err)
	}
	src, _ = s.GetSource(ctx, "vep-113")
	if n := src.Annotations()[0].Name; n != "Allele" {
		t.Errorf(`with prefix "-" got %q, want the bare Allele`, n)
	}
}

// Renaming a source's fields must carry the snapshots that pinned them.
//
// Snapshot defaults are stored denormalized, as plain name strings. So setting a
// prefix used to invalidate every bundle containing that source: the snapshot
// still listed auto_id, materialization emitted CG_auto_id, and every job failed
// with `default_annotations references unknown annotation "auto_id"` — reported
// by varhub at annotate time, with nothing connecting it to the settings form
// that caused it an hour earlier.
func TestPrefixChangeCarriesSnapshotDefaults(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "builtins", Name: "builtins", Version: "1", Kind: "builtin",
		TOML: "[[sources]]\ntype=\"builtin\"\nname=\"builtins\"\nversion=\"1\"\n" +
			"[[sources.annotations]]\nbuiltin=\"auto_id\"\nname=\"auto_id\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	// A second source whose field happens to share the name. Its default must
	// not move: only the renamed source's own fields are rewritten.
	if err := s.PutSource(ctx, Source{
		ID: "other-1", Name: "other", Version: "1", Kind: "vcf", Build: "GRCh38",
		TOML: "[[sources]]\nname=\"other\"\nversion=\"1\"\n" +
			"[[sources.annotations]]\nname=\"auto_id\"\n",
	}); err != nil {
		t.Fatal(err)
	}

	snap := Snapshot{ID: "dev", Build: "GRCh38", Defaults: []string{"auto_id", "KEEP_ME"}}
	if err := s.PutSnapshot(ctx, snap, []string{"builtins"}); err != nil {
		t.Fatal(err)
	}
	other := Snapshot{ID: "other-snap", Build: "GRCh38", Defaults: []string{"auto_id"}}
	if err := s.PutSnapshot(ctx, other, []string{"other-1"}); err != nil {
		t.Fatal(err)
	}

	if err := s.PutSettings(ctx, "builtins", SourceSettings{AnnotationPrefix: "CG_"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSnapshot(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Defaults) != 2 || got.Defaults[0] != "CG_auto_id" {
		t.Errorf("defaults = %v, want the renamed CG_auto_id", got.Defaults)
	}
	if got.Defaults[1] != "KEEP_ME" {
		t.Errorf("unrelated default was touched: %v", got.Defaults)
	}

	// The other snapshot pins a different source that merely shares the name.
	if got, err = s.GetSnapshot(ctx, "other-snap"); err != nil {
		t.Fatal(err)
	}
	if got.Defaults[0] != "auto_id" {
		t.Errorf("another source's default was rewritten on a name match: %v",
			got.Defaults)
	}

	// And back again: clearing the prefix restores the manifest's own names.
	if err := s.PutSettings(ctx, "builtins", SourceSettings{}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetSnapshot(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if got.Defaults[0] != "auto_id" {
		t.Errorf("clearing the prefix left %v", got.Defaults)
	}
}
