package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errReader fails partway, standing in for a client that disconnects mid-upload.
type errReader struct {
	data []byte
	at   int
	fail int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.at >= r.fail {
		return 0, errors.New("connection reset")
	}
	n := copy(p, r.data[r.at:min(r.fail, len(r.data))])
	r.at += n
	return n, nil
}

func TestPutReaderAndOpenRoundTripALocalFile(t *testing.T) {
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "nested", "input.vcf")
	body := "##fileformat=VCFv4.2\nchr1\t100\t.\tA\tT\n"

	if err := PutReader(ctx, dest, strings.NewReader(body)); err != nil {
		t.Fatalf("PutReader: %v", err)
	}
	rc, err := Open(ctx, dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("round trip changed the content:\n got %q\nwant %q", got, body)
	}
}

// A stream that dies partway must leave nothing behind at the destination.
//
// The failure this prevents is the worst kind: a truncated input that reads as a
// valid, shorter VCF. A job would annotate it, report success, and return fewer
// variants than were submitted with nothing anywhere saying so.
func TestAFailedPutReaderLeavesNoFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "input.vcf")

	err := PutReader(ctx, dest, &errReader{data: []byte(strings.Repeat("x", 4096)), fail: 100})
	if err == nil {
		t.Fatal("a reader that fails mid-stream should fail the write")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("the destination exists after a failed write: %v", statErr)
	}
	// And no leftover scratch, which would accumulate one file per failed upload.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind %q", e.Name())
	}
}

func TestDownloadStagesToALocalPath(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src.vcf")
	body := strings.Repeat("chr1\t100\t.\tA\tT\n", 1000)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "work", "input.vcf")
	n, err := Download(ctx, src, dst)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("reported %d bytes, want %d", n, len(body))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("nothing was staged: %v", err)
	}
	if string(got) != body {
		t.Error("the staged file does not match the source")
	}
}

// Staging something that is not there fails, and leaves no partial file for the
// engine to pick up and annotate as if it were the input.
func TestAFailedDownloadLeavesNoPartialFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dst := filepath.Join(dir, "input.vcf")

	if _, err := Download(ctx, filepath.Join(t.TempDir(), "missing.vcf"), dst); err == nil {
		t.Fatal("staging a missing source should fail")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a partial file was left at the destination: %v", err)
	}
}

// The object-store half of the same contract. Skips without a bucket, like the
// rest of this package's S3 tests.
func TestPutReaderStreamsToAnObjectStore(t *testing.T) {
	uri := testBucket(t) + "/stream/input.vcf"
	ctx := context.Background()

	// Past one part, so the multipart path is what runs — the single-PutObject
	// path would pass this test without exercising the buffering that bounds
	// memory.
	body := strings.Repeat("chr1\t100\t.\tA\tT\tPASS\tDP=30\n", 400_000)
	if len(body) <= uploadPartSize {
		t.Fatalf("fixture is %d bytes; it must exceed one %d-byte part", len(body), uploadPartSize)
	}

	if err := PutReader(ctx, uri, strings.NewReader(body)); err != nil {
		t.Fatalf("PutReader: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), uri) })

	rc, err := Open(ctx, uri)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != len(body) {
		t.Fatalf("round trip returned %d bytes, want %d", buf.Len(), len(body))
	}
	if buf.String() != body {
		t.Error("the object's content differs from what was uploaded")
	}
}

func TestDownloadStagesFromAnObjectStore(t *testing.T) {
	uri := testBucket(t) + "/stream/stage.vcf"
	ctx := context.Background()
	body := strings.Repeat("chr2\t200\t.\tC\tG\n", 5000)

	if err := PutReader(ctx, uri, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), uri) })

	dst := filepath.Join(t.TempDir(), "work", "input.vcf")
	n, err := Download(ctx, uri, dst)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("staged %d bytes, want %d", n, len(body))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Error("the staged file does not match the object")
	}
}
