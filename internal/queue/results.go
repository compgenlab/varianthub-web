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

// Variant is one annotated row of a job's results.
type Variant struct {
	Chrom       string         `json:"chrom"`
	Pos         int64          `json:"pos"`
	Ref         string         `json:"ref"`
	Alt         string         `json:"alt"`
	Annotations map[string]any `json:"annotations"`
}

// insertVariants explodes the CLI's result JSON into job_variant rows.
//
// It runs inside the same transaction as the result blob and the status change,
// so a job is never observably done with results that are not yet queryable.
func insertVariants(ctx context.Context, tx pgx.Tx, jobID string, result []byte) error {
	var variants []Variant
	if err := json.Unmarshal(result, &variants); err != nil {
		return fmt.Errorf("parse result for indexing: %w", err)
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
		rows = append(rows, []any{jobID, i, v.Chrom, v.Pos, v.Ref, v.Alt, string(blob)})
	}

	// CopyFrom rather than a row-per-INSERT: a 5,000-variant job is 5,000 network
	// round trips otherwise, which dominates the job's runtime for a fast query.
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"job_variant"},
		[]string{"job_id", "idx", "chrom", "pos", "ref", "alt", "annotations"},
		pgx.CopyFromRows(rows))
	return err
}

// ResultQuery narrows and orders a job's variants.
type ResultQuery struct {
	Search string   // case-insensitive substring across annotation values and the locus
	Sort   string   // "idx" (default) | "locus" | an annotation key
	Desc   bool     //
	Limit  int      // default 100, max 1000
	Offset int      //
	Keys   []string // annotation keys that exist (for validating Sort)
}

// ResultPage is one page of a job's results.
type ResultPage struct {
	Columns []Column  `json:"columns"`
	Rows    []Variant `json:"rows"`
	Total   int       `json:"total"`
	Limit   int       `json:"limit"`
	Offset  int       `json:"offset"`
}

// Column mirrors the stored column model.
type Column struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Type      string `json:"type,omitempty"`
	Source    string `json:"source,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	Default   bool   `json:"default"`
}

// Columns returns a job's stored column model. Nil when the job predates the
// column model or produced no rows.
func (q *Queue) Columns(ctx context.Context, jobID string) ([]Column, error) {
	var raw []byte
	err := q.pool.QueryRow(ctx, `SELECT columns FROM job WHERE id=$1`, jobID).Scan(&raw)
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
// whereArgs and orderArgs are returned separately because the count query has a
// WHERE but no ORDER BY: handing it the sort argument too makes Postgres reject
// the statement for an argument it was never given a placeholder for.
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
		// Match the locus text or any annotation value. jsonb_each_text over one
		// job's rows is cheap at this scale and needs no per-key index.
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
		order = "ORDER BY idx " + dir
	case "locus":
		order = "ORDER BY chrom " + dir + ", pos " + dir + ", idx ASC"
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

// Results returns one page of a job's annotated variants.
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
		`SELECT count(*) FROM job_variant WHERE job_id=$1 `+where, countArgs...).
		Scan(&total); err != nil {
		return ResultPage{}, err
	}

	pageArgs := append(append(append([]any{jobID}, whereArgs...), orderArgs...),
		qy.Limit, qy.Offset)
	sql := fmt.Sprintf(
		`SELECT chrom,pos,ref,alt,annotations FROM job_variant WHERE job_id=$1 %s %s LIMIT $%d OFFSET $%d`,
		where, order, len(pageArgs)-1, len(pageArgs))

	rows, err := q.pool.Query(ctx, sql, pageArgs...)
	if err != nil {
		return ResultPage{}, err
	}
	defer rows.Close()

	page := ResultPage{Columns: cols, Rows: []Variant{}, Total: total,
		Limit: qy.Limit, Offset: qy.Offset}
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
		`SELECT chrom,pos,ref,alt,annotations FROM job_variant WHERE job_id=$1 %s %s`,
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
