package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Variant is one annotated row of a chunk's results.
type Variant struct {
	Chrom       string         `json:"chrom"`
	Pos         int64          `json:"pos"`
	Ref         string         `json:"ref"`
	Alt         string         `json:"alt"`
	Annotations map[string]any `json:"annotations"`
}

// insertVariants writes a chunk's variants as chunk_variant rows.
//
// It runs inside the same transaction as the status change, so a chunk is never
// observably done with results that are not yet queryable.
//
// max bounds how many rows are kept; 0 or less keeps them all. These rows exist
// so a person can page, search and sort through a result in a browser — the
// answer itself is the stored VCF, and every download is served from that. A
// whole genome's worth of JSONB buys a table nobody can use at the size that
// makes it expensive, so it is not written. See catalog.Site.TableRows.
func insertVariants(ctx context.Context, tx pgx.Tx, chunkID string, variants []Variant, max int) error {
	if max > 0 && len(variants) > max {
		// The first max, not a sample: the table is read in order, so the rows
		// somebody sees have to be the ones the file starts with.
		variants = variants[:max]
	}
	if len(variants) == 0 {
		return nil
	}

	rows := make([][]any, 0, len(variants))
	for i, v := range variants {
		ann := v.Annotations
		if ann == nil {
			ann = map[string]any{}
		}
		blob, err := json.Marshal(ann)
		if err != nil {
			return fmt.Errorf("encode annotations for row %d: %w", i, err)
		}
		rows = append(rows, []any{chunkID, i, v.Chrom, v.Pos, v.Ref, v.Alt, string(blob)})
	}

	// CopyFrom rather than a row-per-INSERT: a 5,000-variant chunk is 5,000
	// network round trips otherwise, which dominates the chunk's runtime for a
	// fast query.
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"chunk_variant"},
		[]string{"chunk_id", "idx", "chrom", "pos", "ref", "alt", "annotations"},
		pgx.CopyFromRows(rows))
	return err
}

// ResultQuery narrows and orders a chunk's variants.
type ResultQuery struct {
	Search string   // case-insensitive substring across annotation values and the locus
	Sort   string   // "idx" (default) | "locus" | an annotation key
	Desc   bool     //
	Limit  int      // default 100, max 1000
	Offset int      //
	Keys   []string // annotation keys that exist (for validating Sort)
}

// ResultPage is one page of a chunk's results.
type ResultPage struct {
	Columns []Column  `json:"columns"`
	Rows    []Variant `json:"rows"`
	// Total is how many rows this table holds, which for a large job is fewer
	// than the job annotated — see NVariants.
	Total  int       `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
	// NVariants is how many variants the job actually annotated.
	//
	// Sent alongside Total so a caller can tell a complete table from a window
	// onto the front of a large one. They differ whenever a job ran past the
	// row cap (catalog.Site.TableRows), and without this the difference is
	// invisible: a table of 10,000 rows looks like a job of 10,000 variants,
	// and somebody concludes their submission lost 2.6 million of them.
	NVariants int `json:"n_variants"`
}

// Column mirrors the stored column model.
type Column struct {
	Key   string `json:"key"`
	Label string `json:"label"`

	// Description is the annotation's prose, kept apart from Label. The two
	// were conflated: a described field lost its name in the header, so a table
	// of "PHRED scaled CADD scores" and "ClinVar Aggregate germline
	// classification for this single variant" no longer matched the names the
	// manifest declares, the export writes, or a filter refers to.
	Description string `json:"description,omitempty"`

	Type      string `json:"type,omitempty"`
	Source    string `json:"source,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	Default   bool   `json:"default"`
}

// Columns returns a job's stored column model. Nil when the job predates the
// column model or produced no rows.
//
// Read through the job, which takes them from whichever of its chunks has
// them. Every chunk of a split job describes the same columns — they annotated
// one file against one snapshot — so any will do and there is nothing to
// reconcile.
func (q *Queue) Columns(ctx context.Context, jobID string) ([]Column, error) {
	var raw []byte
	err := q.pool.QueryRow(ctx, `SELECT columns FROM job_state WHERE id=$1`, jobID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cols []Column
	if err := json.Unmarshal(raw, &cols); err != nil {
		return nil, err
	}
	return cols, nil
}

// buildResultSQL assembles the WHERE/ORDER BY for a results query.
//
// whereArgs and orderArgs are returned separately because the count query has
// a WHERE but no ORDER BY: handing it the sort argument too makes Postgres
// reject the statement for an argument it was never given a placeholder for.
// Placeholders are numbered assuming the job id is $1, then whereArgs, then
// orderArgs.
//
// Sort keys are never interpolated raw: an annotation key reaches SQL only as a
// bound parameter to the JSONB accessor, and any other sort value is rejected
// against a fixed set. That keeps a user-supplied column name out of the
// statement text entirely.
func buildResultSQL(qy ResultQuery) (where, order string, whereArgs, orderArgs []any, err error) {
	whereArgs, orderArgs = []any{}, []any{}

	if s := strings.TrimSpace(qy.Search); s != "" {
		// Match the locus text or any annotation value. jsonb_each_text over
		// one chunk's rows is cheap at this scale and needs no per-key index.
		whereArgs = append(whereArgs, "%"+strings.ToLower(s)+"%")
		n := len(whereArgs) + 1 // +1 for the job id at $1
		where = fmt.Sprintf(`AND (
			lower(chrom || ':' || pos || ':' || ref || ':' || alt) LIKE $%d
			OR EXISTS (
				SELECT 1 FROM jsonb_each_text(annotations) kv
				WHERE lower(kv.value) LIKE $%d
			))`, n, n)
	}

	dir := "ASC"
	if qy.Desc {
		dir = "DESC"
	}
	switch qy.Sort {
	case "", "idx":
		// Across the job's chunks, so the piece a row came from leads: idx is
		// only unique within one chunk, and ordering by it alone interleaves
		// twenty-six pieces into a file whose positions jump backwards every
		// hundred thousand rows.
		order = "ORDER BY chunk_index " + dir + " NULLS FIRST, idx " + dir
	case "locus":
		// Natural chromosome order, not lexical. Sorted as text, chr10 lands
		// between chr1 and chr2 and chr7 lands after chr13 — which a reader of
		// genomic data reads as a bug. Numeric contigs sort numerically first,
		// then the rest (X, Y, M, scaffolds) textually after them.
		const chromKey = `regexp_replace(chrom, '^chr', '', 'i')`
		order = fmt.Sprintf(`ORDER BY
			(CASE WHEN %s ~ '^[0-9]+$' THEN %s::int ELSE 1000000 END) %s,
			%s %s, pos %s, idx ASC`,
			chromKey, chromKey, dir, chromKey, dir, dir)
	default:
		if !contains(qy.Keys, qy.Sort) {
			return "", "", nil, nil, fmt.Errorf("unknown sort key %q", qy.Sort)
		}
		orderArgs = append(orderArgs, qy.Sort)
		n := 1 + len(whereArgs) + len(orderArgs) // job id, where args, then this
		// Sort numerically when the value parses as a number, else textually. A
		// numeric annotation sorted as text puts 10 before 9, which is wrong in a
		// way readers notice immediately. NULLS LAST keeps empty cells at the end
		// in both directions — an absent value is not "smallest".
		order = fmt.Sprintf(`ORDER BY
			(CASE WHEN annotations->>$%d ~ '^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?$'
			      THEN (annotations->>$%d)::double precision END) %s NULLS LAST,
			annotations->>$%d %s NULLS LAST, idx ASC`,
			n, n, dir, n, dir)
	}
	return where, order, whereArgs, orderArgs, nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Results returns one page of a job's annotated variants, across every chunk of
// it.
func (q *Queue) Results(ctx context.Context, jobID string, qy ResultQuery) (ResultPage, error) {
	cols, err := q.Columns(ctx, jobID)
	if err != nil {
		return ResultPage{}, err
	}
	if qy.Keys == nil {
		for _, c := range cols {
			qy.Keys = append(qy.Keys, c.Key)
		}
	}
	if qy.Limit <= 0 {
		qy.Limit = 100
	}
	if qy.Limit > 1000 {
		qy.Limit = 1000
	}
	if qy.Offset < 0 {
		qy.Offset = 0
	}

	where, order, whereArgs, orderArgs, err := buildResultSQL(qy)
	if err != nil {
		return ResultPage{}, err
	}

	// The count has a WHERE but no ORDER BY, so it takes only the where args.
	countArgs := append([]any{jobID}, whereArgs...)
	var total int
	if err := q.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+variantsOfJob+` `+where, countArgs...).
		Scan(&total); err != nil {
		return ResultPage{}, err
	}

	pageArgs := append(append(append([]any{jobID}, whereArgs...), orderArgs...),
		qy.Limit, qy.Offset)
	sql := fmt.Sprintf(
		`SELECT chrom,pos,ref,alt,annotations FROM `+variantsOfJob+` %s %s LIMIT $%d OFFSET $%d`,
		where, order, len(pageArgs)-1, len(pageArgs))

	rows, err := q.pool.Query(ctx, sql, pageArgs...)
	if err != nil {
		return ResultPage{}, err
	}
	defer rows.Close()

	// What the job annotated, as against what this table holds. Read from the
	// job rather than counted here: the rows are capped and the count is not.
	var annotated int
	if err := q.pool.QueryRow(ctx,
		`SELECT n_variants FROM job_state WHERE id=$1`, jobID).Scan(&annotated); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return ResultPage{}, err
	}

	page := ResultPage{Columns: cols, Rows: []Variant{}, Total: total,
		Limit: qy.Limit, Offset: qy.Offset, NVariants: annotated}
	for rows.Next() {
		var v Variant
		var ann []byte
		if err := rows.Scan(&v.Chrom, &v.Pos, &v.Ref, &v.Alt, &ann); err != nil {
			return ResultPage{}, err
		}
		if err := json.Unmarshal(ann, &v.Annotations); err != nil {
			return ResultPage{}, err
		}
		page.Rows = append(page.Rows, v)
	}
	return page, rows.Err()
}

// variantsOfJob is the FROM clause every result query shares: a job's rows are
// its chunks' rows.
//
// One string rather than three copies, because the count, the page and the
// stream have to select from exactly the same set — a count taken over a
// different set than the page is a paginator that runs off the end.
const variantsOfJob = `chunk_variant v JOIN chunk c ON c.id = v.chunk_id
	 WHERE c.job_id = $1 `

// StreamResults calls fn for every matching variant in order, in batches, so an
// export never holds a whole result set in memory.
func (q *Queue) StreamResults(ctx context.Context, jobID string, qy ResultQuery,
	fn func(Variant) error) error {

	cols, err := q.Columns(ctx, jobID)
	if err != nil {
		return err
	}
	if qy.Keys == nil {
		for _, c := range cols {
			qy.Keys = append(qy.Keys, c.Key)
		}
	}
	where, order, whereArgs, orderArgs, err := buildResultSQL(qy)
	if err != nil {
		return err
	}
	all := append(append([]any{jobID}, whereArgs...), orderArgs...)

	rows, err := q.pool.Query(ctx, fmt.Sprintf(
		`SELECT chrom,pos,ref,alt,annotations FROM `+variantsOfJob+` %s %s`,
		where, order), all...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var v Variant
		var ann []byte
		if err := rows.Scan(&v.Chrom, &v.Pos, &v.Ref, &v.Alt, &ann); err != nil {
			return err
		}
		if err := json.Unmarshal(ann, &v.Annotations); err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// FormatValue renders an annotation value for a delimited export. JSON numbers
// arrive as float64, so an integer would otherwise print as "1.2e+06".
func FormatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
