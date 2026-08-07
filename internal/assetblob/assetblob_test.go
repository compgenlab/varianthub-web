package assetblob

import (
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// The digest becomes a path component. For a local storage location that means
// a value carrying a slash or a ".." would write outside the asset prefix, so
// it is checked before it is ever joined to a base.
func TestValidDigestRejectsAnythingButHex(t *testing.T) {
	good := strings.Repeat("a1b2c3d4", 8) // 64 hex chars
	if err := validDigest(good); err != nil {
		t.Fatalf("a real digest was rejected: %v", err)
	}
	bad := []string{
		"",
		"short",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		"../../../../etc/cron.d/x" + strings.Repeat("a", 40),
		strings.Repeat("A", 64),  // uppercase: two names for one object
		strings.Repeat("a/", 32), // a path, not a name
		strings.Repeat("z", 64),  // not hex
	}
	for _, d := range bad {
		if err := validDigest(d); err == nil {
			t.Errorf("validDigest(%q) was accepted", d)
		}
	}
	// Length alone must not be the whole test: the traversal attempt above is
	// exactly 64 characters.
	if got := len("../../../../etc/cron.d/x" + strings.Repeat("a", 40)); got != 64 {
		t.Fatalf("the traversal case is %d chars, so it is not testing what it claims", got)
	}
}

// Assets go to a bucket whenever one exists, even when the deployment's default
// location is a path.
//
// A path location may be a shared mount or may be local to one pod, and the
// catalog cannot tell which. Choosing it would work on one node and fail on the
// second replica — the exact failure that moving data to object storage exists
// to avoid.
func TestLocationPrefersObjectStorage(t *testing.T) {
	path := catalog.StorageLocation{ID: "local", Kind: catalog.StoragePath,
		URI: "/mnt/storage", IsDefault: true}
	bucket := catalog.StorageLocation{ID: "s3", Kind: catalog.StorageS3, URI: "s3://vh"}
	other := catalog.StorageLocation{ID: "s3b", Kind: catalog.StorageS3,
		URI: "s3://other", IsDefault: true}
	broken := catalog.StorageLocation{ID: "weird", Kind: "gopher", URI: "gopher://x",
		IsDefault: true}

	cases := []struct {
		name string
		in   []catalog.StorageLocation
		want string
	}{
		{"bucket beats the default path", []catalog.StorageLocation{path, bucket}, "s3"},
		{"order does not matter", []catalog.StorageLocation{bucket, path}, "s3"},
		{"the default bucket wins among buckets",
			[]catalog.StorageLocation{path, bucket, other}, "s3b"},
		{"a path is used when there is no bucket", []catalog.StorageLocation{path}, "local"},
		{"an unusable default is not chosen",
			[]catalog.StorageLocation{broken, path}, "local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := catalog.AssetStorage(tc.in)
			if !ok {
				t.Fatalf("no location chosen from %+v", tc.in)
			}
			if got.ID != tc.want {
				t.Errorf("chose %q, want %q", got.ID, tc.want)
			}
		})
	}

	if _, ok := catalog.AssetStorage(nil); ok {
		t.Error("a location was chosen with none configured")
	}
	if _, ok := catalog.AssetStorage([]catalog.StorageLocation{broken}); ok {
		t.Error("an unusable location was chosen")
	}
}
