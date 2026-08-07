package blob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// s3Bucket is a versitygw (or real S3) bucket to exercise the object path
// against. Skipped when absent, because these assert against a live endpoint
// rather than a stub — a stub would prove our idea of the SDK, not the SDK.
func testBucket(t *testing.T) string {
	t.Helper()
	b := os.Getenv("VHW_TEST_S3_BUCKET")
	if b == "" || os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Skip("VHW_TEST_S3_BUCKET / AWS_ENDPOINT_URL not set; skipping the object-store tests")
	}
	return strings.TrimSuffix(b, "/")
}

// A move copies, verifies, and only then deletes. Every combination of
// filesystem and object store has to work, because a deployment moving off
// pod-local storage uses path→s3, and one consolidating buckets uses s3→s3.
func TestTransferBetweenStorageKinds(t *testing.T) {
	bucket := testBucket(t)
	ctx := context.Background()
	dir := t.TempDir()
	body := []byte("gencode gtf bytes")

	local := filepath.Join(dir, "in", "gencode.gtf.gz")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// path -> s3: the case that moves a deployment off pod-local storage.
	up := bucket + "/movetest/gencode.gtf.gz"
	if n, err := Transfer(ctx, local, up); err != nil {
		t.Fatalf("path->s3: %v", err)
	} else if n != int64(len(body)) {
		t.Errorf("path->s3 reported %d bytes, want %d", n, len(body))
	}
	if !Exists(ctx, up) {
		t.Fatal("path->s3 left nothing in the bucket")
	}

	// s3 -> s3.
	up2 := bucket + "/movetest2/gencode.gtf.gz"
	if _, err := Transfer(ctx, up, up2); err != nil {
		t.Fatalf("s3->s3: %v", err)
	}
	if !Exists(ctx, up2) {
		t.Fatal("s3->s3 left nothing at the destination")
	}

	// s3 -> path, and the content survives the round trip.
	back := filepath.Join(dir, "out", "gencode.gtf.gz")
	if _, err := Transfer(ctx, up2, back); err != nil {
		t.Fatalf("s3->path: %v", err)
	}
	got, err := os.ReadFile(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("content changed across three transfers: %q", got)
	}

	// The source is untouched by a copy — the caller deletes only after the
	// catalog records the new location.
	if _, err := os.Stat(local); err != nil {
		t.Errorf("Transfer removed the original: %v", err)
	}

	// Remove works on both kinds.
	if err := Remove(ctx, up); err != nil {
		t.Errorf("remove s3: %v", err)
	}
	if Exists(ctx, up) {
		t.Error("the object is still there after Remove")
	}
	if err := Remove(ctx, local); err != nil {
		t.Errorf("remove path: %v", err)
	}
	// Absence is not an error: a retried cleanup must not fail.
	if err := Remove(ctx, local); err != nil {
		t.Errorf("removing an absent file errored: %v", err)
	}
}
