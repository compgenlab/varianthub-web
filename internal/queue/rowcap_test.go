package queue

import (
	"context"
	"testing"
)

// The rows kept for the table are capped.
//
// A job's answer is the VCF in storage and every download is served from it;
// these rows exist only so a person can page, search and sort through the result
// in a browser. A chromosome's 2.6M variants is a gigabyte or two of JSONB
// buying a table that is unusable at exactly the size that makes it expensive.
func TestOnlyTheFirstRowsAreKeptForTheTable(t *testing.T) {
	q := testQueue(t)
	seedResultsCapped(t, q, "capped", testBody, testColumns, 2)

	page, err := q.Results(context.Background(), "capped", ResultQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Errorf("kept %d rows against a cap of 2", page.Total)
	}
	// The first two, not any two: the table is read in order, so what somebody
	// sees has to be what the file starts with.
	if len(page.Rows) < 1 || page.Rows[0].Chrom != "chr1" || page.Rows[0].Pos != 100 {
		t.Errorf("first kept row is %+v, want the input's first record", page.Rows[0])
	}
}

// A cap of zero keeps everything, which is what an unbounded setting means.
func TestACapOfZeroKeepsEveryRow(t *testing.T) {
	q := testQueue(t)
	seedResultsCapped(t, q, "uncapped", testBody, testColumns, 0)

	page, err := q.Results(context.Background(), "uncapped", ResultQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 {
		t.Errorf("kept %d of 3 rows with no cap", page.Total)
	}
}

// Only a job's first chunk contributes rows.
//
// The rows are a window onto the start of the result, so the pieces after the
// first have nothing to add — and reading only the first is what lets the cap be
// applied by one worker looking at its own chunk, with no counter shared between
// the twenty-six that may finish at the same moment.
func TestOnlyTheFirstChunkContributesRows(t *testing.T) {
	zero, one := 0, 1
	for _, tc := range []struct {
		name  string
		chunk Chunk
		want  bool
	}{
		{"the sole chunk of an ordinary job", Chunk{}, true},
		{"piece 0 of a split", Chunk{ChunkIndex: &zero}, true},
		{"piece 1 of a split", Chunk{ChunkIndex: &one}, false},
	} {
		if got := firstChunk(tc.chunk); got != tc.want {
			t.Errorf("%s: firstChunk = %v, want %v", tc.name, got, tc.want)
		}
	}
}
