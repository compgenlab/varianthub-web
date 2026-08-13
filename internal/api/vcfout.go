package api

import (
	"log"
	"net/http"

	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// Rendering a VCF for a job with no stored file to serve.
//
// The ordinary answer is the object the worker built, streamed straight out —
// see handleExport. This is what is left when there is none: a job old enough
// that its storage was swept, or one whose worker could not build the file.
//
// A locus list annotated here is still a set of variants, and a VCF is what the
// next tool in somebody's pipeline reads, so the fields a locus list cannot
// supply are written as missing rather than withheld as a reason not to offer
// the format. What this cannot do is return a submitted file's own ID, QUAL,
// FILTER, INFO, FORMAT and sample columns — it never had them. exportMergedVCF
// next door covers that case while the input is still stored.

// exportVCF streams the job's results as a VCF.
func (s *Server) exportVCF(w http.ResponseWriter, job queue.Job,
	cols []queue.Column, rows vcfmerge.Stream) {

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	meta := vcfmerge.Meta{Version: s.cfg.Version, JobID: job.ID, Snapshot: job.Snapshot}
	if err := vcfmerge.Render(w, meta, cols, rows); err != nil {
		// The header is already on the wire, so this cannot become a clean
		// error response. Log it; the client sees a short file.
		log.Printf("api: vcf export %s: %v", job.ID, err)
	}
}
