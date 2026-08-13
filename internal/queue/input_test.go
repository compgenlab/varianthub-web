package queue

import (
	"context"
	"strings"
	"testing"
)

// A small submission still travels inline. Round-tripping a few hundred bytes
// of loci through object storage would cost more than it saves, and the vast
// majority of chunks are that.
func TestASmallInputIsCarriedInline(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Submit(ctx, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	})
	if err != nil {
		t.Fatal(err)
	}

	body, ok, err := q.Input(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("an inline submission reported no body")
	}
	if string(body) != "chr1:1:A:T" {
		t.Errorf("body = %q, want the submitted loci", body)
	}
	if _, stored, err := q.InputRef(ctx, id); err != nil || stored {
		t.Errorf("InputRef says stored=%v (err %v); nothing was uploaded", stored, err)
	}
}

// A stored input is a URI and no bytes, and Input reporting "no body" for it
// is the normal case — not an error, and not something a caller may treat as a
// chunk with nothing in it.
func TestAStoredInputIsAReferenceNotBytes(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	const uri = "s3://varhub-dev/jobs/abc/input.vcf.gz"

	id, err := q.Submit(ctx, NewJob{
		Kind: KindVCF, Snapshot: "s", UserID: "u", InputURI: uri,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, stored, err := q.InputRef(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !stored {
		t.Fatal("a chunk enqueued with a URI reports no stored input")
	}
	if got != uri {
		t.Errorf("InputRef = %q, want %q", got, uri)
	}

	body, ok, err := q.Input(ctx, id)
	if err != nil {
		t.Fatalf("Input on a stored chunk errored: %v", err)
	}
	if ok || body != nil {
		t.Errorf("Input returned %d bytes for a stored input; this process should "+
			"never hold it", len(body))
	}
}

// The claim carries the URI through, because the runner needs to know where to
// stage from and re-reading it would be a second round trip for something the
// claim already touched.
func TestAClaimCarriesTheInputLocation(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	const uri = "s3://varhub-dev/jobs/def/input.vcf"

	if _, err := q.Submit(ctx, NewJob{
		Kind: KindVCF, Snapshot: "s", UserID: "u", InputURI: uri,
	}); err != nil {
		t.Fatal(err)
	}

	chunk, body, ok, err := q.claimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if chunk.InputURI != uri {
		t.Errorf("claimed chunk has InputURI %q, want %q", chunk.InputURI, uri)
	}
	if len(body) != 0 {
		t.Errorf("the claim returned %d bytes; a stored input must not be read here — "+
			"it would hold the claim transaction open for a download", len(body))
	}
}

// Exactly one source, enforced by the database rather than by convention.
//
// Neither is a chunk that can be claimed and then cannot run. Both is two
// inputs with no rule about which one is the submission. Either would be found
// by a worker rather than by the request that caused it.
func TestAChunkInputMustHaveExactlyOneSource(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	_, id := submitJob(t, q, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	})

	// Both: rejected.
	_, err := q.pool.Exec(ctx,
		`UPDATE chunk_input SET uri = 's3://b/k' WHERE chunk_id = $1`, id)
	if err == nil {
		t.Error("a row with both a body and a URI was accepted")
	} else if !strings.Contains(strings.ToLower(err.Error()), "job_input_one_source") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}

	// Neither: rejected.
	_, err = q.pool.Exec(ctx,
		`UPDATE chunk_input SET body = NULL WHERE chunk_id = $1`, id)
	if err == nil {
		t.Error("a row with neither a body nor a URI was accepted")
	} else if !strings.Contains(strings.ToLower(err.Error()), "job_input_one_source") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

// A URI wins over a body when a caller sets both, rather than the insert failing
// on the constraint. The one it went to the trouble of uploading is the one it
// meant.
func TestAURIWinsWhenACallerSetsBoth(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	const uri = "s3://varhub-dev/jobs/ghi/input.vcf"

	id, err := q.Submit(ctx, NewJob{
		Kind: KindVCF, Snapshot: "s", UserID: "u",
		Body: []byte("stale"), InputURI: uri,
	})
	if err != nil {
		t.Fatalf("enqueue with both should not fail: %v", err)
	}
	got, stored, err := q.InputRef(ctx, id)
	if err != nil || !stored || got != uri {
		t.Errorf("InputRef = %q stored=%v err=%v; want the uploaded object", got, stored, err)
	}
	if _, ok, _ := q.Input(ctx, id); ok {
		t.Error("the stale inline body was kept alongside the URI")
	}
}

// An empty submission is still an inline one. NULL would trip the constraint and
// fail with a message about neither column being set, which says nothing about
// the empty input that caused it.
func TestAnEmptyBodyIsStillABody(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()

	id, err := q.Submit(ctx, NewJob{Kind: KindLocus, Snapshot: "s", UserID: "u"})
	if err != nil {
		t.Fatalf("enqueue with no body: %v", err)
	}
	body, ok, err := q.Input(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("an empty submission reported no body at all")
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

// Dropping an input hands back the URI, so the caller can delete the object it
// referred to. Reporting it is the whole point: once the row is gone there is
// nothing left that knows where the object was.
func TestDroppingAnInputReportsTheObjectToDelete(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	const uri = "s3://varhub-dev/jobs/jkl/input.vcf"

	jobID, id := submitJob(t, q, NewJob{
		Kind: KindVCF, Snapshot: "s", UserID: "u", InputURI: uri,
	})

	// Dropped by chunk: the input belongs to the chunk that reads it, and a
	// split job has many. The job is what names it for a caller.
	got, err := q.DropInput(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != uri {
		t.Errorf("DropInput = %q, want %q — the object would be orphaned", got, uri)
	}
	if _, stored, _ := q.InputRef(ctx, jobID); stored {
		t.Error("the input row survived the drop")
	}

	// Dropping an inline input is fine and reports no object.
	_, id2 := submitJob(t, q, NewJob{
		Kind: KindLocus, Snapshot: "s", UserID: "u", Body: []byte("chr1:1:A:T"),
	})
	if got, err := q.DropInput(ctx, id2); err != nil || got != "" {
		t.Errorf("DropInput on an inline chunk = %q (err %v), want no object", got, err)
	}

	// And dropping something already gone is not an error — the sweep and an
	// explicit cleanup can both reach the same chunk.
	if got, err := q.DropInput(ctx, id); err != nil || got != "" {
		t.Errorf("second DropInput = %q (err %v), want a quiet no-op", got, err)
	}
}
