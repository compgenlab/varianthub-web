package queue

import (
	"context"
	"sync"
	"testing"
)

func newJob(t *testing.T, q *Queue, chunks int) (jobID string) {
	t.Helper()
	ctx := context.Background()
	chunkID := enqueueOne(t, q, "u")
	id, err := q.CreateJob(ctx, chunkID, "/tmp/jobs/"+chunkID)
	if err != nil {
		t.Fatal(err)
	}
	if chunks > 0 {
		if err := q.SetChunkCount(ctx, id, chunks); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// A job whose split has not finished is pending, not complete.
//
// Zero of zero chunks reads as "all done" to any comparison that does not know
// better, and a caller shown that would reasonably think their submission had
// finished with no results.
func TestAJobWithNoChunkCountYetIsPending(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newJob(t, q, 0)

	b, ok, err := q.GetJob(ctx, id)
	if err != nil || !ok {
		t.Fatalf("GetJob: %v ok=%v", err, ok)
	}
	if !b.Pending() {
		t.Error("a job that has not been counted should be pending")
	}
	if b.Complete() {
		t.Error("0 of 0 chunks reported as complete")
	}
}

// Exactly one caller is told it was the last, however many finish at once.
//
// This is the whole reason the count is a counter. Asked as a query over
// sibling chunks — "is every chunk terminal yet" — two finishing at the same
// instant each see the other's row already updated and both answer yes, and
// the collect step runs twice: two readers of the same chunks, two uploads to
// the same key, and whichever lands second wins.
func TestExactlyOneChunkIsToldItWasTheLast(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	const chunks = 16
	id := newJob(t, q, chunks)

	var (
		mu    sync.Mutex
		lasts int
		wg    sync.WaitGroup
	)
	for i := 0; i < chunks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			last, err := q.ChunkFinished(ctx, id, true)
			if err != nil {
				t.Errorf("ChunkFinished: %v", err)
				return
			}
			if last {
				mu.Lock()
				lasts++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if lasts != 1 {
		t.Errorf("%d chunks were told they were the last; collect would run that many times", lasts)
	}
	b, _, err := q.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if b.Done != chunks {
		t.Errorf("done = %d, want %d", b.Done, chunks)
	}
}

// A failed chunk still counts toward completion, or a job with one bad chunk
// waits for a collect step that can never start.
func TestAFailedChunkStillCompletesTheJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newJob(t, q, 2)

	if last, err := q.ChunkFinished(ctx, id, false); err != nil || last {
		t.Fatalf("first of two: last=%v err=%v", last, err)
	}
	last, err := q.ChunkFinished(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if !last {
		t.Fatal("the job never completed; collect would never run")
	}

	b, _, err := q.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if b.Failed != 1 || b.Done != 1 {
		t.Errorf("done=%d failed=%d, want 1 and 1", b.Done, b.Failed)
	}
	// Recorded separately so collect can refuse: a joined file missing a chunk
	// is a VCF with a hole in it, and nothing downstream would notice.
	if !b.Complete() {
		t.Error("Complete() is false with every chunk reported")
	}
}

// A chunk that finishes before the split has counted them does not complete
// the job. The split cannot have produced the last chunk if it has not
// finished.
func TestAChunkFinishingBeforeTheCountDoesNotComplete(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newJob(t, q, 0)

	last, err := q.ChunkFinished(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if last {
		t.Error("a job with no chunk count reported completion")
	}
}

// Chunks come back in split order, because joining them any other way produces
// a VCF whose records go backwards.
func TestChunksAreListedInSplitOrder(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newJob(t, q, 3)

	// Enqueued out of order on purpose.
	for _, i := range []int{2, 0, 1} {
		idx := i
		if _, err := q.Enqueue(ctx, NewChunk{
			Kind: KindVCF, Snapshot: "s", UserID: "u",
			InputURI: "s3://b/jobs/x/chunk.vcf.gz",
			JobID:    id, ChunkIndex: &idx,
		}); err != nil {
			t.Fatal(err)
		}
	}

	chunks, err := q.JobChunks(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	for i, c := range chunks {
		if c.ChunkIndex == nil || *c.ChunkIndex != i {
			t.Fatalf("position %d holds chunk %v; the join would reorder the file", i, c.ChunkIndex)
		}
	}
}

// Chunk 0 is a real chunk, not "unset". It is the one carrying the header, so
// confusing the two would produce a joined file with no header at all.
func TestChunkZeroIsDistinctFromNotAChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newJob(t, q, 1)

	zero := 0
	chunkID, err := q.Enqueue(ctx, NewChunk{
		Kind: KindVCF, Snapshot: "s", UserID: "u",
		InputURI: "s3://b/jobs/x/chunk.vcf.gz",
		JobID:    id, ChunkIndex: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	plainID := enqueueOne(t, q, "u")

	chunk, _, err := q.Get(ctx, chunkID)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.ChunkIndex == nil {
		t.Fatal("chunk 0 came back as not-a-chunk")
	}
	if *chunk.ChunkIndex != 0 {
		t.Errorf("chunk index = %d, want 0", *chunk.ChunkIndex)
	}
	plain, _, err := q.Get(ctx, plainID)
	if err != nil {
		t.Fatal(err)
	}
	if plain.ChunkIndex != nil {
		t.Errorf("an ordinary chunk has chunk index %d", *plain.ChunkIndex)
	}
}

// The joined answer is filed against the chunk the submitter was given.
//
// They polled the split chunk and have never heard of the collect chunk.
// Filing it anywhere else means a job that finishes with its file in storage
// and no id that reaches it — a download that returns nothing for a chunk
// reporting done.
func TestTheJoinedAnswerIsReachableFromTheSubmittedChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	submitted := enqueueOne(t, q, "u")
	jobID, err := q.CreateJob(ctx, submitted, "/tmp/jobs/"+submitted)
	if err != nil {
		t.Fatal(err)
	}
	b, ok, err := q.GetJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("GetJob: %v ok=%v", err, ok)
	}

	const joined = "s3://varhub-dev/jobs/x/result.vcf.gz"
	if err := q.SetResultVCF(ctx, b.ChunkID, joined); err != nil {
		t.Fatal(err)
	}

	got, found, err := q.ResultVCF(ctx, submitted)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the chunk the caller holds has no answer; the file is unreachable")
	}
	if got != joined {
		t.Errorf("ResultVCF = %q, want %q", got, joined)
	}
}
