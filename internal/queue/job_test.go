package queue

import (
	"context"
	"sync"
	"testing"
)

// newSplitJob submits a VCF job — whose first chunk is the split — and, when
// chunks > 0, records the count the split would have produced.
func newSplitJob(t *testing.T, q *Queue, chunks int) (jobID string) {
	t.Helper()
	ctx := context.Background()
	id, err := q.Submit(ctx, NewJob{
		Kind: KindSplit, Snapshot: "s", UserID: "u", Label: "cohort.vcf",
		InputURI: "s3://b/jobs/x/input.vcf.gz",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The job is a VCF submission; only its first chunk is a split.
	if j, _, _ := q.GetJob(ctx, id); j.Kind != KindVCF {
		t.Fatalf("a submitted VCF is job kind %q, want %q", j.Kind, KindVCF)
	}
	if chunks > 0 {
		if err := q.SetChunkCount(ctx, id, "/tmp/jobs/"+id, chunks); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// Every submission is a job with at least one chunk.
//
// The whole point of the split: an id a caller holds names a submission, not
// one unit of work, so a locus list and a chromosome answer the same shape.
func TestASubmissionIsAJobWithAChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID := submitOne(t, q, "u")
	j, ok, err := q.GetJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("GetJob: %v ok=%v", err, ok)
	}
	if j.Status != StatusQueued {
		t.Errorf("a fresh job is %q, want %q", j.Status, StatusQueued)
	}
	chunks, err := q.JobChunks(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("a locus submission became %d chunks, want 1", len(chunks))
	}
	c := chunks[0]
	if c.ID == jobID {
		t.Error("the chunk reused the job's id; they are separate things and " +
			"sharing an id hides which one a caller is holding")
	}
	if c.JobID != jobID {
		t.Errorf("the chunk belongs to %q, want %q", c.JobID, jobID)
	}
	if !c.CompletesJob {
		t.Error("the only chunk of a job does not complete it; the job could " +
			"never reach done")
	}
	if j.InputChunkID != c.ID {
		t.Errorf("the job's input is filed under %q, not its chunk %q",
			j.InputChunkID, c.ID)
	}
}

// A job whose split has not finished is pending, not complete.
//
// Zero of zero chunks reads as "all done" to any comparison that does not know
// better, and a caller shown that would reasonably think their submission had
// finished with no results.
func TestAJobWithNoChunkCountYetIsPending(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newSplitJob(t, q, 0)

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
	id := newSplitJob(t, q, chunks)

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
	id := newSplitJob(t, q, 2)

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
	id := newSplitJob(t, q, 0)

	last, err := q.ChunkFinished(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if last {
		t.Error("a job with no chunk count reported completion")
	}
}

// Pieces come back in split order, because joining them any other way produces
// a VCF whose records go backwards.
func TestChunksAreListedInSplitOrder(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newSplitJob(t, q, 3)

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

	chunks, err := q.SplitChunks(ctx, id)
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

// Listing a job's chunks shows the split and the collect too, not only the
// pieces.
//
// A split job that failed says so at the job; which of its chunks failed, and
// whether it was the split, a piece or the join, is only visible here. Hiding
// the brackets would leave a caller able to see that something went wrong and
// nothing about where.
func TestListingAJobsChunksIncludesTheSplitAndCollect(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newSplitJob(t, q, 1)

	idx := 0
	if _, err := q.Enqueue(ctx, NewChunk{
		Kind: KindVCF, Snapshot: "s", UserID: "u",
		InputURI: "s3://b/jobs/x/chunk.vcf.gz",
		JobID:    id, ChunkIndex: &idx,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, NewChunk{
		Kind: KindCollect, Snapshot: "s", UserID: "u",
		JobID: id, CompletesJob: true,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := q.JobChunks(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("the job lists %d chunks, want the split, its piece and the collect", len(all))
	}
	kinds := map[string]bool{}
	for _, c := range all {
		kinds[c.Kind] = true
	}
	for _, want := range []string{KindSplit, KindVCF, KindCollect} {
		if !kinds[want] {
			t.Errorf("no %s chunk in the listing", want)
		}
	}
	pieces, err := q.SplitChunks(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) != 1 {
		t.Errorf("SplitChunks returned %d; it is the pieces only", len(pieces))
	}
}

// A chunk is reachable only underneath the job that owns it.
//
// The route is /jobs/{id}/chunks/{chunkId}, and this is what makes the job's
// entitlement rule the only rule: someone else's chunk is not found rather
// than forbidden, which confirms nothing either way.
func TestAChunkIsNotFoundUnderAnotherJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	mine := submitOne(t, q, "u1")
	theirs := submitOne(t, q, "u2")
	chunks, err := q.JobChunks(ctx, theirs)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := q.JobChunk(ctx, mine, chunks[0].ID); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("another job's chunk was readable through mine")
	}
	if _, ok, err := q.JobChunk(ctx, theirs, chunks[0].ID); err != nil || !ok {
		t.Fatalf("a job's own chunk was not found: ok=%v err=%v", ok, err)
	}
}

// Chunk 0 is a real chunk, not "unset". It is the one carrying the header, so
// confusing the two would produce a joined file with no header at all.
func TestChunkZeroIsDistinctFromNotAChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	id := newSplitJob(t, q, 1)

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

	chunk := getChunk(t, q, chunkID)
	if chunk.ChunkIndex == nil {
		t.Fatal("chunk 0 came back as not-a-chunk")
	}
	if *chunk.ChunkIndex != 0 {
		t.Errorf("chunk index = %d, want 0", *chunk.ChunkIndex)
	}
	if plain := getChunk(t, q, plainID); plain.ChunkIndex != nil {
		t.Errorf("an ordinary chunk has chunk index %d", *plain.ChunkIndex)
	}
}

// The answer is reachable from the job, whichever chunk produced it.
//
// A caller holding a job id has never heard of the collect chunk that built the
// file. Filing the answer anywhere they cannot reach means a job that finishes
// with its result in storage and no id that gets to it — a download returning
// nothing for a job reporting done.
func TestTheJoinedAnswerIsReachableFromTheJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	jobID := newSplitJob(t, q, 1)

	collectID, err := q.Enqueue(ctx, NewChunk{
		Kind: KindCollect, Snapshot: "s", UserID: "u",
		JobID: jobID, CompletesJob: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	const joined = "s3://varhub-dev/jobs/x/result.vcf.gz"
	finishByID(t, q, collectID, StatusDone, "", Outcome{
		Result: []byte("[]"), VCFURI: joined,
	})

	got, found, err := q.ResultVCF(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the job has no answer; the file is unreachable")
	}
	if got != joined {
		t.Errorf("ResultVCF = %q, want %q", got, joined)
	}

	j, _, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusDone {
		t.Errorf("the job is %q after its collect finished, want done", j.Status)
	}
}

// A piece finishing does not finish the job.
//
// The window this closes: between the last piece reporting done and the collect
// being queued, every chunk that exists is terminal. A job whose status was
// aggregated over its chunks would read as done there, and a caller polling
// would fetch a result that has not been assembled.
func TestAPieceFinishingLeavesTheJobRunning(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	jobID := newSplitJob(t, q, 1)

	idx := 0
	pieceID, err := q.Enqueue(ctx, NewChunk{
		Kind: KindVCF, Snapshot: "s", UserID: "u",
		InputURI: "s3://b/jobs/x/chunk.vcf.gz",
		JobID:    jobID, ChunkIndex: &idx,
	})
	if err != nil {
		t.Fatal(err)
	}
	finishByID(t, q, pieceID, StatusDone, "", Outcome{Result: []byte("[]"), N: 7})

	j, _, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Terminal() {
		t.Errorf("the job is %q with its collect still to come", j.Status)
	}
}

// A piece that fails fails the job, and the first failure is the one reported.
//
// There is no answer to assemble once a piece is missing — collect refuses to
// join a file with a gap — so a job left running is a caller waiting for a
// result nobody will produce.
func TestAFailedPieceFailsTheJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	jobID := newSplitJob(t, q, 2)

	for i, outcome := range []struct {
		status, msg string
	}{{StatusError, "reference FASTA missing"}, {StatusError, "a later, different failure"}} {
		idx := i
		id, err := q.Enqueue(ctx, NewChunk{
			Kind: KindVCF, Snapshot: "s", UserID: "u",
			InputURI: "s3://b/jobs/x/chunk.vcf.gz",
			JobID:    jobID, ChunkIndex: &idx,
		})
		if err != nil {
			t.Fatal(err)
		}
		finishByID(t, q, id, outcome.status, outcome.msg, Outcome{})
	}

	j, _, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusError {
		t.Fatalf("the job is %q after a piece failed, want error", j.Status)
	}
	if j.Error != "reference FASTA missing" {
		t.Errorf("the job reports %q; the first failure is the one that explains it", j.Error)
	}
	if j.FinishedAt == 0 {
		t.Error("a failed job has no finish time")
	}
}

// Cancelling a job stops every chunk of it, and says so at once.
//
// A caller who cancels and then polls must not be told the work is still
// running: a running chunk is only signalled, and its worker records the
// outcome whenever it gets the message.
func TestCancellingAJobSettlesItAndItsChunks(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	jobID := newSplitJob(t, q, 2)

	for i := 0; i < 2; i++ {
		idx := i
		if _, err := q.Enqueue(ctx, NewChunk{
			Kind: KindVCF, Snapshot: "s", UserID: "u",
			InputURI: "s3://b/jobs/x/chunk.vcf.gz",
			JobID:    jobID, ChunkIndex: &idx,
		}); err != nil {
			t.Fatal(err)
		}
	}

	j, err := q.CancelJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusCancelled {
		t.Errorf("the job is %q after a cancel", j.Status)
	}

	chunks, err := q.JobChunks(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if c.Status != StatusCancelled {
			t.Errorf("chunk %s (%s) is %q; a queued chunk of a cancelled job "+
				"would still be claimed", c.ID, c.Kind, c.Status)
		}
	}
}

// A chunk reporting in after a cancel does not undo it.
//
// The worker holding a running chunk learns about the cancel over NOTIFY and
// records whatever it saw; that write must not move a job the caller already
// stopped back to done.
func TestAChunkFinishingAfterACancelDoesNotUndoIt(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID := submitOne(t, q, "u")
	chunks, err := q.JobChunks(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CancelJob(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	finishByID(t, q, chunks[0].ID, StatusDone, "", Outcome{Result: []byte("[]"), N: 3})

	j, _, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusCancelled {
		t.Errorf("the job is %q; a cancel was undone by the run it stopped", j.Status)
	}
}
