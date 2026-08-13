package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A job comes back with its chunks, in one call.
//
// The job answers "this failed"; only the chunks answer "in the split", "in
// piece 14 of 26", or "in the join". Fetching them separately would mean a
// second round trip to learn which of twenty-six went wrong, for something the
// first call already had in front of it.
func TestAJobComesBackWithItsChunks(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	seedJob(t, h, "chunky", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53","gnomAD-AF":1,"is_coding":true,"note":null}`})

	w := h.do("GET", "/api/v1/jobs/chunky", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("job = %d: %s", w.Code, w.Body.String())
	}
	var got JobResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.JobID != "chunky" {
		t.Errorf("job_id = %q, want the job asked for", got.JobID)
	}
	if len(got.Chunks) != 1 {
		t.Fatalf("a job that was not split carries %d chunks, want 1", len(got.Chunks))
	}
	c := got.Chunks[0]
	if c.JobID != "chunky" {
		t.Errorf("the chunk claims job %q", c.JobID)
	}
	if c.ChunkID == "chunky" {
		t.Error("the chunk reports the job's id; the two are separate things " +
			"and a caller cannot tell which one they are holding")
	}
	if got.Total != 1 || got.Done != 1 {
		t.Errorf("counts are %d/%d, want 1 done of 1", got.Done, got.Total)
	}
}

// The counts are on every job, split or not.
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
	for _, k := range []string{"chunks", "chunks_total", "chunks_done", "chunks_failed"} {
		if _, ok := m[k]; !ok {
			t.Errorf("job read has no %q; a client cannot show progress "+
				"without asking whether the job was split first", k)
		}
	}
}

// A list is the status without the chunks.
//
// The one place separating them is right: a page of a hundred jobs carrying
// every chunk of each is a response that grows with the wrong thing, and a
// table shows the counts anyway.
func TestAJobListingCarriesCountsButNotChunks(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	_, tok := h.admin(t)

	seedJob(t, h, "listed", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53","gnomAD-AF":1,"is_coding":true,"note":null}`})

	w := h.do("GET", "/api/v1/jobs?kind=all", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("jobs = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Jobs) == 0 {
		t.Fatal("the listing is empty")
	}
	for _, j := range got.Jobs {
		if _, ok := j["chunks"]; ok {
			t.Errorf("a listed job carries its chunks: %v", j)
		}
		if _, ok := j["chunks_total"]; !ok {
			t.Errorf("a listed job has no chunk count: %v", j)
		}
	}
}

// A chunk's log is reachable only under the job that owns it.
//
// Scoping the lookup to the job is what makes the job's entitlement rule the
// only rule: no route takes a chunk id on its own, so there is no second place
// for the ownership check to be forgotten.
func TestAChunkLogOfAnotherJobIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	admin, _ := h.admin(t)
	// A session, not a token: the chunk log is web-only, like the job's.
	sess := h.sessionFor(t, admin.ID)

	seedJob(t, h, "mine", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53","gnomAD-AF":1,"is_coding":true,"note":null}`})
	seedJob(t, h, "theirs", "locus", vcfCols,
		[][4]any{{"chr2", int64(200), "C", "T"}},
		[]string{`{"GENE":"BRCA1","gnomAD-AF":2,"is_coding":true,"note":null}`})

	w := h.doSession("GET", "/api/v1/jobs/mine/chunks/"+chunkOf("theirs")+"/log", sess, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("another job's chunk = %d, want 404: %s", w.Code, w.Body.String())
	}
	// Not found, not forbidden: a status that distinguishes "exists but not
	// yours" from "does not exist" confirms the id of somebody else's work.
	w = h.doSession("GET", "/api/v1/jobs/mine/chunks/no-such-chunk/log", sess, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown chunk = %d, want the same 404", w.Code)
	}
	// Its own is fine.
	w = h.doSession("GET", "/api/v1/jobs/mine/chunks/"+chunkOf("mine")+"/log", sess, nil)
	if w.Code != http.StatusOK {
		t.Errorf("a job's own chunk log = %d: %s", w.Code, w.Body.String())
	}
}
