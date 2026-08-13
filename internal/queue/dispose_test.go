package queue

import (
	"context"
	"testing"
)

// A collected chunk's stored input is handed to the disposer.
//
// Without this the sweep is a leak with a schedule: chunk_input cascades from
// chunk, so removing the row destroys the only record of where the object was.
// The table shrinks on time and the bucket grows forever, and nothing short of
// listing the whole bucket can tell which objects are still owned.
func TestSweepingAChunkOffersItsStoredInputForRemoval(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	const uri = "s3://varhub-dev/jobs/aaa/input.vcf.gz"

	var disposed []string
	q.SetObjectDisposer(func(_ context.Context, uris []string) {
		disposed = append(disposed, uris...)
	})

	id, err := q.Submit(ctx, NewJob{
		Kind: KindVCF, Snapshot: "s", UserID: "u", InputURI: uri,
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	q.finish(ctx, chunk, StatusDone, "", Outcome{})

	if _, err := q.DeleteOlderThan(ctx, 1<<62); err != nil {
		t.Fatal(err)
	}
	if len(disposed) != 1 || disposed[0] != uri {
		t.Fatalf("disposer got %v, want [%s]", disposed, uri)
	}
	// And the row really is gone, so the object could not be found any other way.
	var n int
	if err := q.pool.QueryRow(ctx, `SELECT count(*) FROM chunk WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the chunk row survived the sweep")
	}
}

// A chunk still running is not collected, and neither is its input. The sweep
// takes terminal chunks only; offering a live chunk's input for deletion would
// pull the file out from under the worker reading it.
func TestSweepingLeavesARunningChunksInputAlone(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	var disposed []string
	q.SetObjectDisposer(func(_ context.Context, uris []string) {
		disposed = append(disposed, uris...)
	})

	if _, err := q.Submit(ctx, NewJob{
		Kind: KindVCF, Snapshot: "s", UserID: "u",
		InputURI: "s3://varhub-dev/jobs/bbb/input.vcf",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := q.claimNext(ctx); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// Claimed and not finished: no finished_at, so nothing to collect.
	if _, err := q.DeleteOlderThan(ctx, 1<<62); err != nil {
		t.Fatal(err)
	}
	if len(disposed) != 0 {
		t.Errorf("a running chunk's input was offered for removal: %v", disposed)
	}
}

// An inline chunk has no object, so nothing is offered. Calling the disposer
// with an empty list would have it log a removal that never happened.
func TestSweepingAnInlineChunkOffersNothing(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	called := false
	q.SetObjectDisposer(func(_ context.Context, uris []string) { called = true })

	if _, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	}); err != nil {
		t.Fatal(err)
	}
	chunk, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	q.finish(ctx, chunk, StatusDone, "", Outcome{})

	if _, err := q.DeleteOlderThan(ctx, 1<<62); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("the disposer was called for a chunk with no stored object")
	}
}

// With no disposer set, the sweep still collects the rows. A process without
// storage configured must not stop the queue from ageing out.
func TestSweepingWorksWithNoDisposer(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	if _, err := q.Submit(ctx, NewJob{
		Kind: KindVCF, Snapshot: "s", UserID: "u",
		InputURI: "s3://varhub-dev/jobs/ccc/input.vcf",
	}); err != nil {
		t.Fatal(err)
	}
	chunk, _, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	q.finish(ctx, chunk, StatusDone, "", Outcome{})

	n, err := q.DeleteOlderThan(ctx, 1<<62)
	if err != nil {
		t.Fatalf("the sweep failed with no disposer set: %v", err)
	}
	if n != 1 {
		t.Errorf("collected %d chunks, want 1", n)
	}
}
