package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A job's chunks are reachable underneath it, and say what the job cannot.
//
// The job answers "this failed"; only the chunks answer "in the split", "in
// piece 14 of 26", or "in the join". Before this route existed a caller with a
// split submission could see that something had gone wrong and nothing about
// where, which for a job that took an hour is the whole question.
func TestAJobsChunksAreListedUnderIt(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	seedJob(t, h, "chunky", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53","gnomAD-AF":1,"is_coding":true,"note":null}`})

	w := h.do("GET", "/api/v1/jobs/chunky/chunks", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("chunks = %d: %s", w.Code, w.Body.String())
	}
	var got ChunksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.JobID != "chunky" {
		t.Errorf("job_id = %q, want the job asked for", got.JobID)
	}
	if len(got.Chunks) != 1 {
		t.Fatalf("a job that was not split lists %d chunks, want 1", len(got.Chunks))
	}
	c := got.Chunks[0]
	if c.JobID != "chunky" {
		t.Errorf("the chunk claims job %q", c.JobID)
	}
	if c.ChunkID == "chunky" {
		t.Error("the chunk reports the job's id; the two are separate things " +
			"and a caller cannot tell which one they are holding")
	}

	// And singly, at the id the listing gave.
	w = h.do("GET", "/api/v1/jobs/chunky/chunks/"+c.ChunkID, tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("chunk = %d: %s", w.Code, w.Body.String())
	}
	var one ChunkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.ChunkID != c.ChunkID || one.Status != c.Status {
		t.Errorf("the single read disagrees with the listing: %+v vs %+v", one, c)
	}
}

// A chunk is only reachable under the job that owns it.
//
// Scoping the lookup to the job is what makes the job's entitlement rule the
// only rule: there is no route that takes a chunk id on its own, so there is no
// second place for the ownership check to be forgotten.
func TestAChunkOfAnotherJobIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	seedJob(t, h, "mine", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53","gnomAD-AF":1,"is_coding":true,"note":null}`})
	seedJob(t, h, "theirs", "locus", vcfCols,
		[][4]any{{"chr2", int64(200), "C", "T"}},
		[]string{`{"GENE":"BRCA1","gnomAD-AF":2,"is_coding":true,"note":null}`})

	w := h.do("GET", "/api/v1/jobs/mine/chunks/"+chunkOf("theirs"), tok, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("another job's chunk = %d, want 404: %s", w.Code, w.Body.String())
	}
	// Not found, not forbidden: a status that distinguishes "exists but not
	// yours" from "does not exist" confirms the id of somebody else's work.
	w = h.do("GET", "/api/v1/jobs/mine/chunks/no-such-chunk", tok, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown chunk = %d, want the same 404", w.Code)
	}
}

// The chunks of a job that does not exist are not a different answer from the
// job not existing.
func TestChunksOfAnUnknownJobAreNotFound(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	if w := h.do("GET", "/api/v1/jobs/nope/chunks", tok, nil); w.Code != http.StatusNotFound {
		t.Errorf("chunks of an unknown job = %d, want 404", w.Code)
	}
}

// Every submission reports a chunk count, whether or not it was split.
//
// A client that has to ask "was this split?" before it can read the progress
// fields has two shapes to handle; one chunk of one is the same shape as
// twenty-six of twenty-six.
func TestAJobStatusAlwaysCarriesItsChunkCounts(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	seedJob(t, h, "counted", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53","gnomAD-AF":1,"is_coding":true,"note":null}`})

	w := h.do("GET", "/api/v1/jobs/counted", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"chunks", "chunks_done", "chunks_failed"} {
		if _, ok := m[k]; !ok {
			t.Errorf("job status has no %q; a client cannot show progress "+
				"without asking whether the job was split first", k)
		}
	}
}
