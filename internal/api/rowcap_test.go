package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A capped table says how much it is a window onto.
//
// The rows are bounded so a whole genome does not land in Postgres; the count of
// what was actually annotated is not. Sending only the row count would make a
// table of 10,000 indistinguishable from a job of 10,000 — and somebody would
// conclude their 2.6M-variant submission had lost most of itself.
func TestAResultsPageReportsWhatTheJobAnnotated(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	admin, _ := h.admin(t)
	// A session, not a token: the results table is the web application's, and a
	// bearer token gets a 404 from it.
	sess := h.sessionFor(t, admin.ID)

	// Two rows in the table, but the chunk says it annotated far more — the
	// shape a job past the row cap leaves behind.
	seedJob(t, h, "windowed", "vcf", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}, {"chr1", int64(200), "C", "T"}},
		[]string{`{"GENE":"TP53"}`, `{"GENE":"BRCA1"}`})
	setChunkVariants(t, h, "windowed", 2_614_881)

	w := h.doSession("GET", "/api/v1/jobs/windowed/results", sess, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("results = %d: %s", w.Code, w.Body.String())
	}
	var page struct {
		Total     int `json:"total"`
		NVariants int `json:"n_variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Errorf("total = %d, want the 2 rows the table holds", page.Total)
	}
	if page.NVariants != 2_614_881 {
		t.Errorf("n_variants = %d, want what the job annotated", page.NVariants)
	}
}

// And an uncapped job reports the same number twice, so a client can tell the
// two cases apart by comparing rather than by knowing the cap.
func TestAnUncappedResultsPageAgreesWithItself(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	admin, _ := h.admin(t)
	sess := h.sessionFor(t, admin.ID)

	seedJob(t, h, "whole", "locus", vcfCols,
		[][4]any{{"chr1", int64(100), "A", "G"}},
		[]string{`{"GENE":"TP53"}`})
	setChunkVariants(t, h, "whole", 1)

	w := h.doSession("GET", "/api/v1/jobs/whole/results", sess, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("results = %d: %s", w.Code, w.Body.String())
	}
	var page struct {
		Total     int `json:"total"`
		NVariants int `json:"n_variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	// Non-zero, or this passes on an empty body — 0 == 0 is not agreement.
	if page.Total == 0 {
		t.Fatal("no rows came back; the comparison below would be vacuous")
	}
	if page.Total != page.NVariants {
		t.Errorf("total=%d n_variants=%d; a complete table should report the same "+
			"number twice", page.Total, page.NVariants)
	}
}
