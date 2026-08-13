package blob

import (
	"context"
	"strings"
	"testing"
)

// The object-store half of List, against a live endpoint.
//
// Worth its own test because the two implementations share nothing: one walks a
// directory, the other pages an API. A sweep that silently saw only the first
// page of a bucket would leave everything after it forever, which is the bug
// this shape of code has.
func TestListPagesAnObjectStore(t *testing.T) {
	bucket := testBucket(t)
	ctx := context.Background()
	prefix := bucket + "/listtest/"

	want := []string{"jobs/a/input.vcf", "jobs/b/input.vcf", "jobs/b/result.vcf"}
	for _, name := range want {
		if err := PutReader(ctx, prefix+name, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = Remove(context.Background(), prefix+name) })
	}

	got, err := List(ctx, prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]bool{}
	for _, o := range got {
		seen[strings.TrimPrefix(o.URI, prefix)] = true
		if o.URI == "" || !strings.HasPrefix(o.URI, "s3://") {
			t.Errorf("listed %q, which is not a usable locator", o.URI)
		}
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("%s missing from the listing (%d objects seen)", name, len(got))
		}
	}
}
