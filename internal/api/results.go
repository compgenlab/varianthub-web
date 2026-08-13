package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// resultsReady checks a job is finished and returns the right status otherwise.
// Queued/running is 409 and failure is 422 — the caller asked for something that
// does not exist yet versus something that never will.
func (s *Server) resultsReady(w http.ResponseWriter, job queue.Job) bool {
	switch job.Status {
	case queue.StatusQueued, queue.StatusRunning:
		writeError(w, http.StatusConflict,
			"job is not finished (status: "+job.Status+")")
		return false
	case queue.StatusError:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "job failed", "detail": job.Error, "job_id": job.ID,
		})
		return false
	}
	return true
}

// resultQuery reads paging, sorting and search from the query string.
func resultQuery(r *http.Request) queue.ResultQuery {
	q := r.URL.Query()
	perPage := clampInt(q.Get("per_page"), 100, 1, 1000)
	page := clampInt(q.Get("page"), 1, 1, 1<<30)

	// page/per_page and limit/offset both work; the design's table pages, while
	// scripted consumers usually want offsets.
	limit, offset := perPage, (page-1)*perPage
	if raw := q.Get("limit"); raw != "" {
		limit = clampInt(raw, 100, 1, 1000)
		offset = clampInt(q.Get("offset"), 0, 0, 1<<30)
	}

	order := strings.ToLower(strings.TrimSpace(q.Get("order")))
	return queue.ResultQuery{
		Search: q.Get("q"),
		Sort:   strings.TrimSpace(q.Get("sort")),
		Desc:   order == "desc",
		Limit:  limit,
		Offset: offset,
	}
}

// handleResults returns one page of a job's annotated variants, with the column
// definitions needed to render them.
func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	job, ok := s.job(w, r)
	if !ok {
		return
	}
	if !s.resultsReady(w, job) {
		return
	}

	page, err := s.queue.Results(r.Context(), job.ID, resultQuery(r))
	if err != nil {
		// An unknown sort key is the caller's mistake, not a server fault.
		if strings.Contains(err.Error(), "unknown sort key") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleExport streams a job's whole result set — not just the current page —
// honoring the active search and sort.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	job, ok := s.job(w, r)
	if !ok {
		return
	}
	if !s.resultsReady(w, job) {
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		// A job submitted as a VCF defaults to a VCF: the caller had one, so
		// giving rows back and making them ask for their own format again is a
		// question with an obvious answer.
		if job.Kind == queue.KindVCF {
			format = "vcf"
		} else {
			format = "json"
		}
	}
	switch format {
	case "json", "tsv", "csv", "vcf":
	default:
		writeError(w, http.StatusBadRequest,
			"unknown format "+format+" (want json, tsv, csv or vcf)")
		return
	}

	cols, err := s.queue.Columns(r.Context(), job.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	qy := resultQuery(r)
	qy.Limit, qy.Offset = 0, 0 // export is the whole matching set

	filename := "variants-" + job.ID[:min(8, len(job.ID))] + "." + format
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	// A VCF download is the stored object itself, decompressed on the way out.
	// No parse, no render, no column model — the bytes the worker wrote are the
	// answer, and for a submitted file they are that file with the annotations
	// added, samples and all.
	if format == "vcf" {
		if rc, ok := s.openStoredResult(r, job); ok {
			defer rc.Close()
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if _, err := io.Copy(w, rc); err != nil {
				logExportFailure(job.ID, err)
			}
			return
		}
		// Nothing stored, so this will be rendered from rows below. Coordinate
		// order, whatever was asked for: a VCF sorted by CADD is not a VCF
		// anything can index, and it would look perfectly fine until someone ran
		// tabix on it. The search filter is still honoured — that changes which
		// records appear, not their order.
		qy.Sort, qy.Desc = "locus", false
	}

	rows, release := s.rowSource(r, job, qy)
	defer release()

	// Headers go out before the first row, so a mid-stream database error cannot
	// be turned into a clean error response. Nothing is buffered — a large export
	// must not be held in memory to preserve that option.
	switch format {
	case "json":
		s.exportJSON(w, job, rows)
	case "tsv":
		s.exportDelimited(w, job, cols, rows, '\t')
	case "csv":
		s.exportDelimited(w, job, cols, rows, ',')
	case "vcf":
		// Nothing stored to serve. A submitted VCF is still answered with its
		// own file annotated, which keeps the ID, QUAL, FILTER, existing INFO,
		// FORMAT and sample columns the caller sent; anything else is rendered
		// from rows. So a download degrades rather than fails.
		if job.Kind == queue.KindVCF && s.exportMergedVCF(w, r, job, cols, qy) {
			return
		}
		s.exportVCF(w, job, cols, rows)
	}
}

func (s *Server) exportJSON(w http.ResponseWriter, job queue.Job, rows vcfmerge.Stream) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	first := true
	fmt.Fprint(w, "[")
	err := rows(func(v queue.Variant) error {
		if !first {
			fmt.Fprint(w, ",")
		}
		first = false
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	})
	fmt.Fprint(w, "]")
	if err != nil {
		// The array is already open on the wire; the client sees truncated JSON,
		// which is at least detectable. Log the cause for operators.
		logExportFailure(job.ID, err)
	}
}

func (s *Server) exportDelimited(w http.ResponseWriter, job queue.Job,
	cols []queue.Column, rows vcfmerge.Stream, sep rune) {

	if sep == '\t' {
		w.Header().Set("Content-Type", "text/tab-separated-values; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	cw.Comma = sep
	defer cw.Flush()

	header := []string{"chrom", "pos", "ref", "alt"}
	for _, c := range cols {
		header = append(header, c.Key)
	}
	if err := cw.Write(header); err != nil {
		logExportFailure(job.ID, err)
		return
	}

	err := rows(func(v queue.Variant) error {
		row := []string{v.Chrom, fmt.Sprint(v.Pos), v.Ref, v.Alt}
		// Iterate the column model, not the map: column order must match the
		// header, and map iteration order does not.
		for _, c := range cols {
			row = append(row, queue.FormatValue(v.Annotations[c.Key]))
		}
		if err := cw.Write(row); err != nil {
			return err
		}
		// Flush periodically so a slow client sees progress and memory stays flat.
		cw.Flush()
		return cw.Error()
	})
	if err != nil {
		logExportFailure(job.ID, err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// logExportFailure records an export that died mid-stream. The response is
// already committed by then, so this is the only place the cause is visible.
func logExportFailure(jobID string, err error) {
	log.Printf("api: export of job %s failed mid-stream: %v", jobID, err)
}
