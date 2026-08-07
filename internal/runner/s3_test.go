package runner

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// s3Bucket is a versitygw (or real S3) bucket to exercise the object path
// against. Skipped when absent, because these assert against a live endpoint
// rather than a stub — a stub would prove our idea of the SDK, not the SDK.
func s3Bucket(t *testing.T) string {
	t.Helper()
	b := os.Getenv("VHW_TEST_S3_BUCKET")
	if b == "" || os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Skip("VHW_TEST_S3_BUCKET / AWS_ENDPOINT_URL not set; skipping the object-store tests")
	}
	return strings.TrimSuffix(b, "/")
}

// A reference can be fetched from a bucket and copied back to one, with the
// digest verified the same way as for http. Both directions matter: the source
// URI may be s3://, and the durable copy always is when the picker names one.
func TestS3RoundTripWithChecksum(t *testing.T) {
	bucket := s3Bucket(t)
	ctx := context.Background()
	dir := t.TempDir()

	body := []byte(">chr1\nACGTACGTAC\nACGT\n")
	sum := md5.Sum(body)
	local := filepath.Join(dir, "ref.fa")
	if err := os.WriteFile(local, body, 0o644); err != nil {
		t.Fatal(err)
	}

	uri := bucket + "/reftest/ref.fa"
	if err := CopyTo(ctx, local, uri); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if !s3Exists(ctx, uri) {
		t.Fatal("uploaded object is not there")
	}

	back := filepath.Join(dir, "back.fa")
	n, err := FetchFile(ctx, uri, back, "md5:"+hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("FetchFile from s3: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("fetched %d bytes, want %d", n, len(body))
	}
	got, err := os.ReadFile(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("content changed in transit")
	}
}

// A digest that disagrees must leave nothing behind — an unverified reference is
// worse than a missing one, because a tool annotates against it and says nothing.
func TestS3FetchRejectsBadChecksum(t *testing.T) {
	bucket := s3Bucket(t)
	ctx := context.Background()
	dir := t.TempDir()

	local := filepath.Join(dir, "x.fa")
	if err := os.WriteFile(local, []byte("actual bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := bucket + "/reftest/x.fa"
	if err := CopyTo(ctx, local, uri); err != nil {
		t.Fatal(err)
	}

	wrong := md5.Sum([]byte("what the manifest promised"))
	dest := filepath.Join(dir, "out.fa")
	if _, err := FetchFile(ctx, uri, dest, "md5:"+hex.EncodeToString(wrong[:])); err == nil {
		t.Fatal("a mismatched digest was accepted")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error does not name the mismatch: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a file was left behind despite the digest disagreeing")
	}
}

// The durable copy exists so another worker can restore instead of re-fetching,
// which means the indexes have to travel with it: a copy that restores to
// something a tool still has to index is only half a copy.
func TestRestoreFromBringsTheIndexes(t *testing.T) {
	bucket := s3Bucket(t)
	ctx := context.Background()
	src, dst := t.TempDir(), t.TempDir()

	for name, body := range map[string]string{
		"ref.fa.gz":     "data",
		"ref.fa.gz.fai": "chr1\t14\t6\t10\t11\n",
		"ref.fa.gz.gzi": "\x00\x00\x00\x00\x00\x00\x00\x00",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	durable := bucket + "/reftest/restore/ref.fa.gz"
	for _, ext := range []string{"", ".fai", ".gzi"} {
		if err := CopyTo(ctx, filepath.Join(src, "ref.fa.gz"+ext), durable+ext); err != nil {
			t.Fatal(err)
		}
	}

	local, ok := RestoreFrom(ctx, durable, dst)
	if !ok {
		t.Fatal("restore reported failure")
	}
	for _, ext := range []string{"", ".fai", ".gzi"} {
		if _, err := os.Stat(local + ext); err != nil {
			t.Errorf("restore did not bring %s: %v", filepath.Base(local+ext), err)
		}
	}
}

// Nothing to restore is an ordinary answer, not an error: the caller falls back
// to fetching from the source.
func TestRestoreFromReportsAbsence(t *testing.T) {
	bucket := s3Bucket(t)
	if _, ok := RestoreFrom(context.Background(), bucket+"/reftest/nope/ref.fa.gz", t.TempDir()); ok {
		t.Error("reported a restore from an object that is not there")
	}
}
