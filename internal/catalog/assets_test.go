package catalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const revelTOML = `[[sources]]
name = "revel"
version = "1.3"
format = "tab"
assembly = "GRCh38"

  [sources.build]
  output = "merged.revel.txt.gz"
  inputs = ["https://example.org/revel.zip"]
  assets = ["convert_csv_to_tab.py"]
  run = ["python3 {workdir}/convert_csv_to_tab.py"]
`

func TestAssetNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want []string
	}{
		{"build recipe asset", revelTOML, []string{"convert_csv_to_tab.py"}},
		{
			name: "tool step assets",
			toml: "[[sources]]\nname=\"vep\"\nversion=\"1\"\ntype=\"tool\"\n" +
				"assets = [\"expand.py\", \"worst.py\"]\n",
			want: []string{"expand.py", "worst.py"},
		},
		{
			// varhub fetches a URL asset itself at run time, so it does not
			// travel with the manifest and must not be looked for beside it.
			name: "url assets are not co-located",
			toml: "[[sources]]\nname=\"x\"\nversion=\"1\"\n" +
				"assets = [\"https://example.org/s.py\", \"local.py\"]\n",
			want: []string{"local.py"},
		},
		{"no assets", "[[sources]]\nname=\"x\"\nversion=\"1\"\n", nil},
		{"unparseable", "not toml at all {{{", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AssetNames(tc.toml)
			if len(got) != len(tc.want) {
				t.Fatalf("AssetNames = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("AssetNames[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Asset names come from a manifest, and a manifest can come from a registry
// nobody here controls. A name that escapes its directory must be refused
// before anything writes it.
func TestValidateAssetNameRefusesEscapes(t *testing.T) {
	for _, bad := range []string{
		"", "../evil.py", "../../etc/passwd", "/etc/passwd",
		"sub/../../out.py", "https://example.org/x.py", "./x.py",
	} {
		if err := ValidateAssetName(bad); err == nil {
			t.Errorf("ValidateAssetName(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"convert.py", "scripts/convert.py", "a/b/c.sh"} {
		if err := ValidateAssetName(ok); err != nil {
			t.Errorf("ValidateAssetName(%q) = %v, want nil", ok, err)
		}
	}
}

func TestAssetRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "revel-1.3", Name: "revel", Version: "1.3", Kind: "tab",
		Build: "GRCh38", TOML: revelTOML,
	}); err != nil {
		t.Fatal(err)
	}
	body := "#!/usr/bin/env python3\nprint('hi')\n"
	if err := s.PutAssets(ctx, "revel-1.3", []Asset{{Name: "convert_csv_to_tab.py", Content: body}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Assets(ctx, "revel-1.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != body {
		t.Fatalf("Assets = %+v", got)
	}

	// Replaced, not merged: the manifest decides which assets exist, so one
	// left over from an earlier version would shadow nothing and confuse.
	if err := s.PutAssets(ctx, "revel-1.3", []Asset{{Name: "other.py", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Assets(ctx, "revel-1.3")
	if len(got) != 1 || got[0].Name != "other.py" {
		t.Errorf("after replace: %+v", got)
	}

	// A stored name that would escape is refused even here, so the only way to
	// get one into the database is not through this API.
	if err := s.PutAssets(ctx, "revel-1.3", []Asset{{Name: "../x.py", Content: "x"}}); err == nil {
		t.Error("stored an asset name that escapes the source directory")
	}
	// ...and the refusal did not half-apply.
	if got, _ = s.Assets(ctx, "revel-1.3"); len(got) != 1 || got[0].Name != "other.py" {
		t.Errorf("a rejected write changed the stored set: %+v", got)
	}
}

func TestMissingAssets(t *testing.T) {
	if got := MissingAssets(revelTOML, nil); len(got) != 1 || got[0] != "convert_csv_to_tab.py" {
		t.Errorf("MissingAssets with none supplied = %v", got)
	}
	have := []Asset{{Name: "convert_csv_to_tab.py", Content: "x"}}
	if got := MissingAssets(revelTOML, have); len(got) != 0 {
		t.Errorf("MissingAssets with the file supplied = %v", got)
	}
}

// The whole point: a materialized home has the script where the recipe looks.
func TestMaterializeWritesAssets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutSource(ctx, Source{
		ID: "revel-1.3", Name: "revel", Version: "1.3", Kind: "tab",
		Build: "GRCh38", TOML: revelTOML,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAssets(ctx, "revel-1.3",
		[]Asset{{Name: "convert_csv_to_tab.py", Content: "print('convert')\n"}}); err != nil {
		t.Fatal(err)
	}

	m := &Materializer{
		Store: s, Root: t.TempDir(),
		DataDir:  "/var/lib/varianthub/data",
		CacheDir: "/var/lib/varianthub/sources",
	}
	home, cleanup, err := m.HomeForSources(ctx, []string{"revel-1.3"})
	if err != nil {
		t.Fatalf("HomeForSources: %v", err)
	}
	defer cleanup()

	// The path varhub resolves for a "name:version" ref.
	p := filepath.Join(home, "annotations", "sources", "revel", "1.3", "convert_csv_to_tab.py")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("asset not materialized: %v", err)
	}
	if !strings.Contains(string(body), "convert") {
		t.Errorf("asset content = %q", body)
	}
	// A build step executes it, so the mode has to allow that.
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("asset is not executable: %04o", info.Mode().Perm())
	}
}
