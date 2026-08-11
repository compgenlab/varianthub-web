package catalog

import (
	"context"
	"testing"
)

const streamedTOML = `[[sources]]
  type    = "vcf"
  name    = "remote"
  version = "1"
  stream  = true
  url     = "https://example.invalid/remote.vcf.gz"

  [[sources.annotations]]
    name  = "af"
    field = "AF"
    type  = "numeric"

  [[sources.annotations]]
    name  = "note"
    field = "NOTE"
`

// A manifest that names its own prefix, so the effective name and the manifest
// name differ before this deployment overrides anything.
const prefixedTOML = `[[sources]]
  type              = "vcf"
  name              = "vep"
  version           = "113"
  annotation_prefix = "VEP_"
  url               = "https://example.invalid/vep.vcf.gz"
  stream            = true

  [[sources.annotations]]
    name  = "VEP_consequence"
    field = "Consequence"
`

func fieldsFixture(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	for _, src := range []Source{
		{ID: "builtins", Name: "builtins", Version: "1", Kind: "builtin",
			Visibility: VisibilityPublic, TOML: builtinsTOML},
		{ID: "remote", Name: "remote", Version: "1", Kind: "vcf", Build: "GRCh38",
			Visibility: VisibilityPublic, TOML: streamedTOML},
		{ID: "vep", Name: "vep", Version: "113", Kind: "tool", Build: "GRCh38",
			Visibility: VisibilityPublic, TOML: prefixedTOML},
	} {
		if err := s.PutSource(ctx, src); err != nil {
			t.Fatalf("PutSource %s: %v", src.ID, err)
		}
	}
	if err := s.PutSnapshot(ctx, Snapshot{
		ID: "snap", Build: "GRCh38", State: StatePublished,
		Defaults: []string{"auto_id", "af"},
	}, []string{"builtins", "remote", "vep"}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	return s
}

func byName(fields []Field) map[string]Field {
	out := map[string]Field{}
	for _, f := range fields {
		out[f.Name] = f
	}
	return out
}

func TestSnapshotFieldsAttributesEveryField(t *testing.T) {
	s := fieldsFixture(t)
	snap, fields, err := s.SnapshotFields(context.Background(), "snap")
	if err != nil {
		t.Fatalf("SnapshotFields: %v", err)
	}
	// The assembly comes back with the fields because a cached value cannot be
	// keyed without it.
	if snap.Build != "GRCh38" {
		t.Errorf("Build = %q, want GRCh38", snap.Build)
	}

	got := byName(fields)
	for _, want := range []struct {
		name, manifest, ref, builtin string
	}{
		{"auto_id", "auto_id", "builtins:1", "auto_id"},
		{"tstv", "tstv", "builtins:1", "tstv"},
		{"af", "af", "remote:1", ""},
		{"note", "note", "remote:1", ""},
	} {
		f, ok := got[want.name]
		if !ok {
			t.Errorf("field %q missing", want.name)
			continue
		}
		if f.Manifest != want.manifest {
			t.Errorf("%s: Manifest = %q, want %q", want.name, f.Manifest, want.manifest)
		}
		if f.SourceRef != want.ref {
			t.Errorf("%s: SourceRef = %q, want %q", want.name, f.SourceRef, want.ref)
		}
		if f.Builtin != want.builtin {
			t.Errorf("%s: Builtin = %q, want %q", want.name, f.Builtin, want.builtin)
		}
	}
}

// The whole point of caching under the manifest's name: a prefix rename changes
// what results are called without touching what has already been computed.
func TestSnapshotFieldsKeepsTheManifestNameThroughAPrefixChange(t *testing.T) {
	s := fieldsFixture(t)
	ctx := context.Background()

	_, fields, err := s.SnapshotFields(ctx, "snap")
	if err != nil {
		t.Fatalf("SnapshotFields: %v", err)
	}
	f, ok := byName(fields)["VEP_consequence"]
	if !ok {
		t.Fatal("VEP_consequence missing before the rename")
	}
	if f.Manifest != "VEP_consequence" {
		t.Fatalf("Manifest = %q, want VEP_consequence", f.Manifest)
	}

	if err := s.PutSettings(ctx, "vep", SourceSettings{AnnotationPrefix: "VEP113_"}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	_, fields, err = s.SnapshotFields(ctx, "snap")
	if err != nil {
		t.Fatalf("SnapshotFields after rename: %v", err)
	}
	f, ok = byName(fields)["VEP113_consequence"]
	if !ok {
		t.Fatal("the renamed field did not come back under its new name")
	}
	if f.Manifest != "VEP_consequence" {
		t.Errorf("Manifest = %q after the rename; a cached value would be orphaned by it", f.Manifest)
	}
	if f.SourceRef != "vep:113" {
		t.Errorf("SourceRef = %q, want vep:113", f.SourceRef)
	}
}

func TestSnapshotFieldsClassifiesCost(t *testing.T) {
	s := fieldsFixture(t)
	_, fields, err := s.SnapshotFields(context.Background(), "snap")
	if err != nil {
		t.Fatalf("SnapshotFields: %v", err)
	}
	got := byName(fields)
	// A builtin computes from the variant and reads nothing. It has no storage
	// root, and an unknown root must not make the cheapest thing look costly.
	if got["auto_id"].Expensive {
		t.Error("a builtin was classified as expensive")
	}
	// Streamed: every query is a range request against somebody else's server.
	if !got["af"].Expensive {
		t.Error("a streamed source was classified as cheap")
	}
	// A tool means starting a container per invocation.
	if !got["VEP_consequence"].Expensive {
		t.Error("a tool source was classified as cheap")
	}
}

func TestLocalRootRecognizesOnlyLocalPaths(t *testing.T) {
	local := []string{"/var/lib/varianthub/data", "/mnt/ref"}
	for _, r := range local {
		if !localRoot(r) {
			t.Errorf("localRoot(%q) = false, want true", r)
		}
	}
	// Everything else falls on the expensive side — including a scheme nobody
	// has thought of, which is the case an exclusion list gets wrong by calling
	// it local and issuing a range request per locus against a third party.
	remote := []string{
		"", "s3://bucket/prefix", "https://example.invalid/data",
		"gs://bucket", "webdav+ssh://host/path", "relative/path",
	}
	for _, r := range remote {
		if localRoot(r) {
			t.Errorf("localRoot(%q) = true, want false", r)
		}
	}
}
