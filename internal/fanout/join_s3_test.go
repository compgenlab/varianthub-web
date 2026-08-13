package fanout

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/blob"
)

// The join against an object store, which is what it is for.
//
// The claim this package rests on is that chunks go from storage to storage
// without touching a disk. Proving it on the filesystem proves the logic and
// not the claim — the object path is a different code path in blob, and it is
// the one production uses.
func TestJoinStreamsBetweenObjects(t *testing.T) {
	bucket := strings.TrimSuffix(os.Getenv("VHW_TEST_S3_BUCKET"), "/")
	if bucket == "" || os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Skip("VHW_TEST_S3_BUCKET / AWS_ENDPOINT_URL not set; skipping the object-store test")
	}
	ctx := context.Background()
	prefix := bucket + "/fanout/"

	var uris []string
	for i, body := range []string{chunk(100, 3), chunk(200, 2)} {
		raw := bgzipped(t, body)
		if i > 0 {
			var stripped bytes.Buffer
			if _, err := StripHeader(bytes.NewReader(raw), &stripped); err != nil {
				t.Fatal(err)
			}
			raw = stripped.Bytes()
		}
		uri := prefix + ChunkName(i+1)
		if err := blob.PutReader(ctx, uri, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = blob.Remove(context.Background(), uri) })
		uris = append(uris, uri)
	}

	dest := prefix + "joined.vcf.gz"
	if err := Join(ctx, uris, dest); err != nil {
		t.Fatalf("Join: %v", err)
	}
	t.Cleanup(func() { _ = blob.Remove(context.Background(), dest) })

	rc, err := blob.Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("the joined object is not gzip: %v", err)
	}
	defer gz.Close()

	rd, err := vcf.NewVcfReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Header(); err != nil {
		t.Fatalf("no readable header: %v", err)
	}
	var n int
	for {
		if _, err := rd.NextRecord(); err != nil {
			break
		}
		n++
	}
	if n != 5 {
		t.Errorf("read %d records from the joined object, want 5", n)
	}
}
