package queue

import (
	"context"
	"testing"
)

// splitJob submits a VCF job and returns the job id and its split chunk's id.
func splitJob(t *testing.T, q *Queue) (jobID, splitID string) {
	t.Helper()
	return submitJob(t, q, NewJob{
		Kind: KindSplit, Snapshot: "s", UserID: "u", Label: "cohort.vcf",
		InputURI: "s3://b/jobs/x/input.vcf.gz",
	})
}

// addPieces queues n pieces and the join that waits for them, the way a split
// does.
func addPieces(t *testing.T, q *Queue, jobID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	for i := 0; i < n; i++ {
		idx := i
		id, err := q.Enqueue(ctx, NewChunk{
			Kind: KindVCF, Snapshot: "s", UserID: "u",
			InputURI: "s3://b/jobs/x/chunk.vcf.gz",
			JobID:    jobID, ChunkIndex: &idx,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := q.Enqueue(ctx, NewChunk{
		Kind: KindCollect, Snapshot: "s", UserID: "u", JobID: jobID,
		AwaitsPieces: true, CompletesJob: true,
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

func statusOfJob(t *testing.T, q *Queue, jobID string) Job {
	t.Helper()
	j, ok, err := q.GetJob(context.Background(), jobID)
	if err != nil || !ok {
		t.Fatalf("GetJob: %v ok=%v", err, ok)
	}
	return j
}

// Every submission is a job with at least one chunk.
//
// The whole point of the split: an id a caller holds names a submission, not
// one unit of work, so a locus list and a chromosome answer the same shape.
func TestASubmissionIsAJobWithAChunk(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID := submitOne(t, q, "u")
	j := statusOfJob(t, q, jobID)
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
	if j.InputChunkID != c.ID {
		t.Errorf("the job's input is filed under %q, not its chunk %q",
			j.InputChunkID, c.ID)
	}
	if j.Chunks != 1 || j.Done != 0 {
		t.Errorf("counts are %d done of %d, want 0 of 1", j.Done, j.Chunks)
	}
}

// A job of one chunk follows that chunk, with no partial states in between.
//
// "partial" is a property of a thing made of parts. A submission that was not
// split has none, so it goes queued → running → done exactly as its chunk does.
func TestAJobOfOneChunkFollowsIt(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, chunkID := submitJob(t, q, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	})
	if got := statusOfJob(t, q, jobID).Status; got != StatusQueued {
		t.Fatalf("before the claim the job is %q, want queued", got)
	}
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if got := statusOfJob(t, q, jobID).Status; got != StatusRunning {
		t.Fatalf("with its chunk claimed the job is %q, want running", got)
	}
	finishByID(t, q, chunkID, StatusDone, "", Outcome{N: 4})

	j := statusOfJob(t, q, jobID)
	if j.Status != StatusDone {
		t.Fatalf("the job is %q after its only chunk finished", j.Status)
	}
	if j.NVariants != 4 {
		t.Errorf("n_variants = %d, want the chunk's 4", j.NVariants)
	}
	if j.ResultChunkID != chunkID {
		t.Errorf("the answer is filed under %q, want the chunk %q", j.ResultChunkID, chunkID)
	}
	if j.FinishedAt == 0 || j.StartedAt == 0 {
		t.Errorf("a finished job has no times: started=%d finished=%d", j.StartedAt, j.FinishedAt)
	}
}

// A fan-out reports partial states, because that is what it is.
//
// Nine of twenty-six pieces annotated is neither "running" nor "queued" in any
// useful sense, and reporting it as either is how a caller ends up counting
// chunks themselves.
func TestAFanOutReportsPartialStates(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, splitID := splitJob(t, q)
	finishByID(t, q, splitID, StatusDone, "", Outcome{})
	pieces := addPieces(t, q, jobID, 2)

	// One of four chunks done, nothing running.
	if got := statusOfJob(t, q, jobID).Status; got != StatusPartialQueued {
		t.Fatalf("after the split the job is %q, want %q", got, StatusPartialQueued)
	}

	// Something finished, something running.
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("claim a piece: %v ok=%v", err, ok)
	}
	if got := statusOfJob(t, q, jobID).Status; got != StatusPartialRunning {
		t.Fatalf("with a piece running the job is %q, want %q", got, StatusPartialRunning)
	}

	for _, id := range pieces {
		finishByID(t, q, id, StatusDone, "", Outcome{N: 3})
	}

	// Every piece done, the join still to run. This is the moment a status
	// aggregated over "is every chunk terminal" would call done, and there is
	// no answer to fetch.
	j := statusOfJob(t, q, jobID)
	if j.Status != StatusPartialQueued {
		t.Fatalf("with the join still queued the job is %q, want %q", j.Status, StatusPartialQueued)
	}
	if j.Terminal() {
		t.Error("a job whose answer has not been assembled reports as terminal")
	}
}

// The join does not run until every piece is done.
//
// It is queued with the pieces, so it is a row a worker can see from the start
// — and the claim has to be what keeps it back, or a worker joins a set of
// files that is still being written.
func TestTheJoinWaitsForItsPieces(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, splitID := splitJob(t, q)
	finishByID(t, q, splitID, StatusDone, "", Outcome{})
	pieces := addPieces(t, q, jobID, 2)

	// Two pieces claimable, and nothing else, however many times we ask.
	claimed := map[string]bool{}
	for i := 0; i < 3; i++ {
		c, _, ok, err := q.claimNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if c.Kind == KindCollect {
			t.Fatal("the join was claimed with pieces still unfinished")
		}
		claimed[c.ID] = true
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d chunks, want the two pieces", len(claimed))
	}

	// One piece done is not enough.
	finishByID(t, q, pieces[0], StatusDone, "", Outcome{N: 1})
	if c, _, ok, err := q.claimNext(ctx); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("claimed %s (%s) with a piece still running", c.ID, c.Kind)
	}

	// Both done, and now it is.
	finishByID(t, q, pieces[1], StatusDone, "", Outcome{N: 1})
	c, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("the join never became claimable: ok=%v err=%v", ok, err)
	}
	if c.Kind != KindCollect {
		t.Fatalf("claimed a %s, want the join", c.Kind)
	}
}

// Only one worker ever gets the join.
//
// It is queued once and claimed like anything else, so exactly-once comes from
// the claim rather than from a counter deciding who is allowed to queue it.
// Asking twice must find nothing the second time.
func TestTheJoinIsClaimedOnce(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, splitID := splitJob(t, q)
	finishByID(t, q, splitID, StatusDone, "", Outcome{})
	for _, id := range addPieces(t, q, jobID, 2) {
		finishByID(t, q, id, StatusDone, "", Outcome{N: 1})
	}

	first, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok || first.Kind != KindCollect {
		t.Fatalf("first claim: %s ok=%v err=%v", first.Kind, ok, err)
	}
	if c, _, ok, err := q.claimNext(ctx); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("a second worker also claimed %s (%s); the join would run twice",
			c.ID, c.Kind)
	}
}

// The answer is reachable from the job, whichever chunk produced it, and the
// count is the pieces' rather than the join's.
func TestTheJoinedAnswerIsReachableFromTheJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, splitID := splitJob(t, q)
	finishByID(t, q, splitID, StatusDone, "", Outcome{})
	for _, id := range addPieces(t, q, jobID, 2) {
		finishByID(t, q, id, StatusDone, "", Outcome{N: 5})
	}
	collect, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim the join: ok=%v err=%v", ok, err)
	}

	const joined = "s3://varhub-dev/jobs/x/result.vcf.gz"
	finishByID(t, q, collect.ID, StatusDone, "", Outcome{
		VCFURI: joined,
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

	j := statusOfJob(t, q, jobID)
	if j.Status != StatusDone {
		t.Errorf("the job is %q after its join finished, want done", j.Status)
	}
	// The join annotated nothing; the pieces did.
	if j.NVariants != 10 {
		t.Errorf("n_variants = %d, want the 10 its pieces annotated", j.NVariants)
	}
}

// A piece that fails fails the job, and the first failure is the one reported.
//
// There is no answer to assemble once a piece is missing — the join refuses to
// produce a file with a gap — so a job left running is a caller waiting for a
// result nobody will produce.
func TestAFailedPieceFailsTheJob(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, splitID := splitJob(t, q)
	finishByID(t, q, splitID, StatusDone, "", Outcome{})
	pieces := addPieces(t, q, jobID, 2)
	finishByID(t, q, pieces[0], StatusError, "reference FASTA missing", Outcome{})
	finishByID(t, q, pieces[1], StatusError, "a later, different failure", Outcome{})

	j := statusOfJob(t, q, jobID)
	if j.Status != StatusError {
		t.Fatalf("the job is %q after a piece failed, want error", j.Status)
	}
	if j.Error != "reference FASTA missing" {
		t.Errorf("the job reports %q; the first failure is the one that explains it", j.Error)
	}
	if j.FinishedAt == 0 {
		t.Error("a failed job has no finish time")
	}
	// And the join never runs: it waits for every piece to be *done*, and two
	// of them never will be.
	if c, _, ok, err := q.claimNext(ctx); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("claimed %s (%s) for a job that cannot be joined", c.ID, c.Kind)
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

	jobID, splitID := splitJob(t, q)
	finishByID(t, q, splitID, StatusDone, "", Outcome{})
	addPieces(t, q, jobID, 2)

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
		if c.Status == StatusQueued {
			t.Errorf("chunk %s (%s) is still queued; it would be claimed", c.ID, c.Kind)
		}
	}
	if c, _, ok, err := q.claimNext(ctx); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("claimed %s (%s) from a cancelled job", c.ID, c.Kind)
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

	jobID, chunkID := submitJob(t, q, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	})
	if _, err := q.CancelJob(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	finishByID(t, q, chunkID, StatusDone, "", Outcome{N: 3})

	if got := statusOfJob(t, q, jobID).Status; got != StatusCancelled {
		t.Errorf("the job is %q; a cancel was undone by the run it stopped", got)
	}
}

// Listing a job's chunks shows the split and the join too, not only the pieces.
//
// A split job that failed says so at the job; which of its chunks failed, and
// whether it was the split, a piece or the join, is only visible here.
func TestListingAJobsChunksIncludesTheSplitAndJoin(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	jobID, splitID := splitJob(t, q)
	finishByID(t, q, splitID, StatusDone, "", Outcome{})
	addPieces(t, q, jobID, 1)

	all, err := q.JobChunks(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("the job lists %d chunks, want the split, its piece and the join", len(all))
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
	pieces, err := q.SplitChunks(ctx, jobID)
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

	jobID, _ := splitJob(t, q)
	pieces := addPieces(t, q, jobID, 1)
	plainID := enqueueOne(t, q, "u")

	chunk := getChunk(t, q, pieces[0])
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
