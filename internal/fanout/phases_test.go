package fanout

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// fakeQueue records what the phases asked of it, in order.
type fakeQueue struct {
	batchID  string
	prefix   string
	count    int
	countSet bool
	enqueued []queue.NewJob
	chunks   []queue.Job
	batch    queue.Batch
	// order records the sequence of calls, so a test can assert that the chunk
	// count was written after the chunks were queued rather than before.
	order []string
}

func (f *fakeQueue) CreateBatch(_ context.Context, jobID, prefix string) (string, error) {
	f.batchID, f.prefix = "batch-1", prefix
	f.batch = queue.Batch{ID: f.batchID, JobID: jobID, Prefix: prefix}
	f.order = append(f.order, "create")
	return f.batchID, nil
}

func (f *fakeQueue) SetChunkCount(_ context.Context, _ string, n int) error {
	f.count, f.countSet = n, true
	f.batch.Chunks = n
	f.order = append(f.order, "count")
	return nil
}

func (f *fakeQueue) Enqueue(_ context.Context, j queue.NewJob) (string, error) {
	f.enqueued = append(f.enqueued, j)
	f.order = append(f.order, "enqueue")
	return "job-" + j.Label, nil
}

func (f *fakeQueue) BatchChunks(context.Context, string) ([]queue.Job, error) {
	return f.chunks, nil
}

func (f *fakeQueue) GetBatch(context.Context, string) (queue.Batch, bool, error) {
	return f.batch, true, nil
}

// A split queues one job per chunk, in order, each pointing at its own stored
// chunk — and records the count only once they all exist.
func TestSplitQueuesAJobPerChunk(t *testing.T) {
	bin := cgkitBin(t)
	dir := t.TempDir()
	store := t.TempDir()

	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(chunk(100, 10)), 0o600); err != nil {
		t.Fatal(err)
	}

	q := &fakeQueue{}
	job := queue.Job{ID: "abc123", Snapshot: "s", Label: "cohort.vcf"}
	batchID, n, err := RunSplit(context.Background(), q, job, in, store, bin, 4, nil)
	if err != nil {
		t.Fatalf("RunSplit: %v", err)
	}
	if n != 3 || batchID == "" {
		t.Fatalf("split into %d chunks, batch %q", n, batchID)
	}
	if len(q.enqueued) != 3 {
		t.Fatalf("queued %d jobs, want 3", len(q.enqueued))
	}

	for i, j := range q.enqueued {
		if j.ChunkIndex == nil || *j.ChunkIndex != i {
			t.Errorf("job %d has chunk index %v, want %d", i, j.ChunkIndex, i)
		}
		if j.BatchID != batchID {
			t.Errorf("job %d belongs to %q, want %q", i, j.BatchID, batchID)
		}
		if j.Kind != queue.KindVCF {
			t.Errorf("job %d is kind %q; a chunk is an ordinary VCF job", i, j.Kind)
		}
		// Its own chunk, not the whole submission.
		want := ChunkName(i + 1)
		if !strings.HasSuffix(j.InputURI, want) {
			t.Errorf("job %d reads %q, want it to end in %s", i, j.InputURI, want)
		}
		if _, err := os.Stat(strings.TrimPrefix(j.InputURI, "")); err != nil {
			t.Errorf("job %d's chunk was not stored: %v", i, err)
		}
	}

	// The count is written last. Written first, the earliest chunk to finish
	// could complete a batch whose remaining chunks had not been queued yet,
	// and collect would join a file that was still being built.
	if !q.countSet || q.count != 3 {
		t.Fatalf("chunk count = %d set=%v", q.count, q.countSet)
	}
	if last := q.order[len(q.order)-1]; last != "count" {
		t.Errorf("the chunk count was written at %q, not last: %v", last, q.order)
	}
}

// The chunks a split stores are what a collect joins: same prefix, same names.
// If these two disagree the batch produces nothing and says nothing about why.
func TestSplitAndCollectAgreeOnWhereChunksLive(t *testing.T) {
	bin := cgkitBin(t)
	dir := t.TempDir()
	store := t.TempDir()
	ctx := context.Background()

	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(chunk(100, 6)), 0o600); err != nil {
		t.Fatal(err)
	}
	q := &fakeQueue{}
	job := queue.Job{ID: "abc123", Snapshot: "s", Label: "cohort.vcf"}
	if _, _, err := RunSplit(ctx, q, job, in, store, bin, 3, nil); err != nil {
		t.Fatal(err)
	}

	// Each chunk annotated and stored, as the chunk jobs would do.
	for i := range q.enqueued {
		body := chunk(100+i*3, 3)
		if _, err := StoreChunkResult(ctx, q.prefix, i, []byte(body), nil); err != nil {
			t.Fatalf("store chunk %d: %v", i, err)
		}
		idx := i
		q.chunks = append(q.chunks, queue.Job{ChunkIndex: &idx})
	}
	q.batch.Failed = 0

	dest, err := RunCollect(ctx, q, q.batchID, store, nil)
	if err != nil {
		t.Fatalf("RunCollect: %v", err)
	}

	f, err := os.Open(dest)
	if err != nil {
		t.Fatalf("the joined file is not where collect said: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	rd, err := vcf.NewVcfReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Header(); err != nil {
		t.Fatalf("the joined file has no header: %v", err)
	}
	var n int
	for {
		if _, err := rd.NextRecord(); err != nil {
			break
		}
		n++
	}
	if n != 6 {
		t.Errorf("the joined file holds %d records, want 6", n)
	}
}

// A batch with a failed chunk is not joined.
//
// The result would be a VCF missing a range of the genome, which reads exactly
// like one where those variants had nothing to say — a wrong answer that looks
// like a right one.
func TestCollectRefusesABatchWithAFailedChunk(t *testing.T) {
	q := &fakeQueue{batchID: "b"}
	q.batch = queue.Batch{ID: "b", Chunks: 3, Done: 2, Failed: 1, Prefix: "/tmp/x"}

	_, err := RunCollect(context.Background(), q, "b", "/tmp", nil)
	if err == nil {
		t.Fatal("a batch with a failed chunk was joined")
	}
	if !strings.Contains(err.Error(), "gap") {
		t.Errorf("the error should say why it refused: %v", err)
	}
}

// Only chunk 0 keeps its header; the rest are stored headerless.
func TestOnlyTheFirstChunkResultKeepsItsHeader(t *testing.T) {
	ctx := context.Background()
	prefix := t.TempDir()

	firstURI, err := StoreChunkResult(ctx, prefix, 0, []byte(chunk(100, 2)), nil)
	if err != nil {
		t.Fatal(err)
	}
	restURI, err := StoreChunkResult(ctx, prefix, 1, []byte(chunk(200, 2)), nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(readGz(t, firstURI), "#CHROM") {
		t.Error("chunk 0 lost its header; the joined file would have none")
	}
	if strings.Contains(readGz(t, restURI), "#") {
		t.Error("a later chunk kept its header; it would appear mid-file")
	}
}

func readGz(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := gz.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}
