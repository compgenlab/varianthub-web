package queue

import (
	"context"
	"strings"
	"testing"
)

// seedResults inserts a finished job with n annotated variants.
func seedResults(t *testing.T, q *Queue, jobID string, body, columns string) {
	t.Helper()
	ctx := context.Background()
	if _, err := q.pool.Exec(ctx, `
		INSERT INTO job (id,kind,snapshot,selection,status,client_ip,created_at,finished_at,columns)
		VALUES ($1,'locus','s','','done','1.1.1.1',1,2,$2)`, jobID, columns); err != nil {
		t.Fatal(err)
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertVariants(ctx, tx, jobID, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

const testColumns = `[
  {"key":"gene","label":"Gene","type":"text","source":"VEP"},
  {"key":"af","label":"gnomAD AF","type":"numeric","source":"gnomAD"}
]`

const testBody = `[
 {"chrom":"chr1","pos":100,"ref":"A","alt":"G","annotations":{"gene":"TP53","af":9}},
 {"chrom":"chr2","pos":50,"ref":"C","alt":"T","annotations":{"gene":"BRCA1","af":10}},
 {"chrom":"chr1","pos":200,"ref":"G","alt":"A","annotations":{"gene":"MSH2","af":null}}
]`

func TestResultsPagingAndColumns(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)
	ctx := context.Background()

	page, err := q.Results(ctx, "j1", ResultQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Results: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("Total = %d, want 3", page.Total)
	}
	if len(page.Rows) != 2 {
		t.Fatalf("got %d rows, want 2 (limit)", len(page.Rows))
	}
	// Default order is the CLI's output order.
	if page.Rows[0].Chrom != "chr1" || page.Rows[0].Pos != 100 {
		t.Errorf("first row = %+v, want chr1:100", page.Rows[0])
	}
	if len(page.Columns) != 2 || page.Columns[0].Source != "VEP" {
		t.Errorf("columns = %+v", page.Columns)
	}

	second, err := q.Results(ctx, "j1", ResultQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != 1 || second.Rows[0].Pos != 200 {
		t.Errorf("second page = %+v", second.Rows)
	}
}

// Sorting an annotation must be numeric when the values are numbers. Sorted as
// text, 10 sorts before 9 — wrong in a way a reader notices immediately.
func TestResultsNumericSort(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)

	page, err := q.Results(context.Background(), "j1", ResultQuery{Sort: "af"})
	if err != nil {
		t.Fatalf("Results: %v", err)
	}
	var got []any
	for _, r := range page.Rows {
		got = append(got, r.Annotations["af"])
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0] != float64(9) || got[1] != float64(10) {
		t.Errorf("ascending af = %v, want 9 then 10 (numeric, not lexical)", got)
	}
	// A null annotation sorts last, not first — an empty cell is not "smallest".
	if got[2] != nil {
		t.Errorf("null af should sort last, got %v", got)
	}
}

func TestResultsTextSortAndDesc(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)

	page, err := q.Results(context.Background(), "j1", ResultQuery{Sort: "gene", Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	if page.Rows[0].Annotations["gene"] != "TP53" {
		t.Errorf("descending gene = %v, want TP53 first", page.Rows[0].Annotations["gene"])
	}
}

func TestResultsSortByLocus(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)

	page, err := q.Results(context.Background(), "j1", ResultQuery{Sort: "locus"})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		chrom string
		pos   int64
	}{{"chr1", 100}, {"chr1", 200}, {"chr2", 50}}
	for i, w := range want {
		if page.Rows[i].Chrom != w.chrom || page.Rows[i].Pos != w.pos {
			t.Errorf("row %d = %s:%d, want %s:%d", i,
				page.Rows[i].Chrom, page.Rows[i].Pos, w.chrom, w.pos)
		}
	}
}

// An unknown sort key must be rejected rather than silently ignored — and it
// must never reach the statement text.
func TestResultsRejectsUnknownSort(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)

	_, err := q.Results(context.Background(), "j1", ResultQuery{Sort: "nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown sort key") {
		t.Fatalf("err = %v, want an unknown-sort-key error", err)
	}
	// A SQL-shaped key is rejected by the same check, so it never gets near the
	// query. Proven by the table still being queryable afterwards.
	if _, err := q.Results(context.Background(), "j1",
		ResultQuery{Sort: `idx; DROP TABLE job_variant --`}); err == nil {
		t.Fatal("a SQL-shaped sort key should be rejected")
	}
	page, err := q.Results(context.Background(), "j1", ResultQuery{})
	if err != nil || page.Total != 3 {
		t.Fatalf("table damaged: total=%d err=%v", page.Total, err)
	}
}

func TestResultsSearch(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)
	ctx := context.Background()

	// Matches an annotation value, case-insensitively.
	page, err := q.Results(ctx, "j1", ResultQuery{Search: "brca"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Rows[0].Annotations["gene"] != "BRCA1" {
		t.Errorf("search 'brca' → total=%d rows=%+v", page.Total, page.Rows)
	}

	// Matches the locus text.
	page, err = q.Results(ctx, "j1", ResultQuery{Search: "chr2:50"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Rows[0].Chrom != "chr2" {
		t.Errorf("search by locus → total=%d rows=%+v", page.Total, page.Rows)
	}

	// Total reflects the filter, not the whole set — otherwise the pager lies.
	page, err = q.Results(ctx, "j1", ResultQuery{Search: "zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Rows) != 0 {
		t.Errorf("no-match search → total=%d rows=%d", page.Total, len(page.Rows))
	}
}

func TestStreamResultsCoversWholeSet(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)

	var n int
	err := q.StreamResults(context.Background(), "j1", ResultQuery{Limit: 1},
		func(Variant) error { n++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	// Streaming ignores Limit: an export is the whole matching set, not a page.
	if n != 3 {
		t.Errorf("streamed %d variants, want all 3", n)
	}
}

// Deleting a job must take its variants with it, or GC would leave orphans.
func TestVariantsCascadeOnJobDelete(t *testing.T) {
	q := testQueue(t)
	seedResults(t, q, "j1", testBody, testColumns)
	ctx := context.Background()

	if _, err := q.DeleteOlderThan(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := q.pool.QueryRow(ctx,
		`SELECT count(*) FROM job_variant WHERE job_id='j1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d orphaned variant rows after job GC", n)
	}
}

func TestFormatValue(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"x", "x"},
		{true, "true"},
		{float64(1200000), "1200000"}, // not 1.2e+06
		{float64(0.0001), "0.0001"},
		{float64(1.5), "1.5"},
		{[]any{"a", "b"}, `["a","b"]`},
	} {
		if got := FormatValue(tc.in); got != tc.want {
			t.Errorf("FormatValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
