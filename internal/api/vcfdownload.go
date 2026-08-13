package api

import (
	"io"
	"log"
	"net/http"

	"github.com/compgenlab/cghts/htsio/bgzf"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// Answering format=vcf, in the order of what it costs.
//
// A finished job's answer is already a file in the object store. The best thing
// this service can do with a request for it is get out of the way: sign a link
// and redirect, so the bytes go from the store to the caller without passing
// through here at all. A chromosome's annotated VCF relayed through this process
// is read out of the store, buffered, and written out again — paid twice, held
// open for as long as the client is slow, and multiplied by everyone downloading
// at once.
//
// Three sources, and every one of them produces the same thing: a BGZF VCF.
// That matters more than it looks. The stored object is compressed, so a
// redirect can only ever deliver it compressed; if the relayed path decompressed
// and the redirect did not, one endpoint would return two different files
// depending on how the deployment happened to be configured. That is the
// divergence this whole change exists to remove, so the answer is BGZF whichever
// path served it — which is also the form every downstream tool wants, since a
// VCF worth this much is one somebody is going to index.

// gzipContentType is what a stored result is, and what all three paths emit.
const gzipContentType = "application/gzip"

// exportVCFResult answers a request for a job's annotated VCF.
func (s *Server) exportVCFResult(w http.ResponseWriter, r *http.Request, job queue.Job,
	cols []queue.Column, qy queue.ResultQuery, filename string) {

	if uri, ok := s.resultURI(r, job); ok {
		// A link straight to the object. Minted only now, after s.job() has
		// established this caller may have this job: the URL carries its own
		// authority and is not checked again by anything.
		url, signed, err := blob.Presign(r.Context(), uri, blob.PresignTTL,
			blob.Disposition{Filename: filename, ContentType: gzipContentType})
		if err != nil {
			// Never fatal. A link that could not be signed costs bandwidth, and
			// the object is still right there to be relayed.
			log.Printf("api: job %s: presign %s: %v", job.ID, uri, err)
		}
		if signed {
			http.Redirect(w, r, url, http.StatusFound)
			return
		}
		if s.relayStoredResult(w, r, job, uri) {
			return
		}
	}

	// Nothing stored, or it has gone. Rebuild it — slower, still correct.
	//
	// Coordinate order, whatever was asked for: a VCF sorted by CADD is not a
	// VCF anything can index, and it would look perfectly fine until someone ran
	// tabix on it. A search filter is still honoured; that changes which records
	// appear, not their order.
	qy.Sort, qy.Desc = "locus", false

	w.Header().Set("Content-Type", gzipContentType)
	w.WriteHeader(http.StatusOK)
	if err := gzipTo(w, func(z io.Writer) error {
		// A submitted VCF is answered with its own file annotated, which keeps
		// the ID, QUAL, FILTER, existing INFO, FORMAT and sample columns the
		// caller sent. Only when there is no stored input left to merge onto
		// does this fall back to a rendering that never had them.
		if job.Kind == queue.KindVCF {
			if done, mErr := s.writeMergedVCF(z, r, job, cols, qy); done {
				return mErr
			}
		}
		rows, release := s.rowSource(r, job, qy)
		defer release()
		meta := vcfmerge.Meta{Version: s.cfg.Version, JobID: job.ID, Snapshot: job.Snapshot}
		return vcfmerge.Render(z, meta, cols, rows)
	}); err != nil {
		// The response is committed, so this can only truncate. Logged, because
		// it is the only place the cause is visible.
		logExportFailure(job.ID, err)
	}
}

// relayStoredResult streams the stored object through this service, reporting
// whether it could.
func (s *Server) relayStoredResult(w http.ResponseWriter, r *http.Request,
	job queue.Job, uri string) bool {

	rc, err := blob.Open(r.Context(), uri)
	if err != nil {
		log.Printf("api: job %s: open result %s, rebuilding instead: %v", job.ID, uri, err)
		return false
	}
	defer rc.Close()

	w.Header().Set("Content-Type", gzipContentType)
	w.WriteHeader(http.StatusOK)

	// Told by the name, not sniffed from the bytes. A stored result is gzipped
	// and has been since it was given a name that says so; anything else is
	// compressed on the way out so that the name the caller saves it under is
	// the truth about the file.
	if queue.Compressed(uri) {
		if _, cErr := io.Copy(w, rc); cErr != nil {
			logExportFailure(job.ID, cErr)
		}
		return true
	}
	if cErr := gzipTo(w, func(z io.Writer) error {
		_, e := io.Copy(z, rc)
		return e
	}); cErr != nil {
		logExportFailure(job.ID, cErr)
	}
	return true
}

// gzipTo runs write against a BGZF compressor over w and closes it.
//
// Closing is what flushes the final block and writes the EOF marker, and it is
// separate from any error write returned: a body that stops early is at least an
// unterminated stream, which a reader detects, rather than one that looks
// complete.
func gzipTo(w io.Writer, write func(io.Writer) error) error {
	z := bgzf.NewWriter(w)
	if err := write(z); err != nil {
		z.Close()
		return err
	}
	return z.Close()
}
