package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"net/http"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// Answering a VCF submission with the submitter's own file, annotated.
//
// The rendered-from-rows VCF next door is a fine answer for a locus list, which
// never had a file. It is a poor one here: it returns a skeleton carrying only
// the columns this server knows about, so a submitted ID, QUAL, FILTER, INFO,
// FORMAT and every sample column are dropped. Someone who sent a two-sample
// tumour/normal VCF got back two bare loci.
//
// So the stored input is re-read and the annotations are set on its records.
// cghts does the parsing and writing: an unmodified record round-trips
// verbatim and a modified one is rebuilt from its parsed model, which is what
// makes everything this server does not care about survive untouched.

// openJobInput returns the VCF a job was submitted with, from wherever it is,
// decompressed if it was stored compressed. ok is false when there is nothing to
// read — a job swept long ago, or one that never had a file.
//
// Two shapes because there are two: submissions from before job storage existed
// carry their bytes in Postgres, and everything since is an object. Both have to
// keep working, or every job queued before the deploy loses its VCF export.
//
// Compression is read off the stored name rather than sniffed from the bytes.
// The upload handler classified it once, when the file arrived, and recorded the
// answer in that name; this is a consumer being told, not a fifth place deciding
// for itself.
func (s *Server) openJobInput(r *http.Request, job queue.Chunk) (io.Reader, func(), bool) {
	noop := func() {}

	uri, stored, err := s.queue.InputRef(r.Context(), job.ID)
	if err != nil {
		log.Printf("api: job %s: read input location: %v", job.ID, err)
		return nil, noop, false
	}
	if !stored {
		// Inline, and therefore small and uncompressed: this path predates
		// storage, and nothing ever wrote a gzipped body into Postgres.
		body, ok, err := s.queue.Input(r.Context(), job.ID)
		if err != nil || !ok || len(body) == 0 {
			return nil, noop, false
		}
		return bytes.NewReader(body), noop, true
	}

	rc, err := blob.Open(r.Context(), uri)
	if err != nil {
		log.Printf("api: job %s: open input %s: %v", job.ID, uri, err)
		return nil, noop, false
	}
	if !queue.Compressed(uri) {
		return rc, func() { rc.Close() }, true
	}
	gz, err := gzip.NewReader(rc)
	if err != nil {
		// Named .gz and not gzip. Standing aside rather than serving the raw
		// bytes as if they were a VCF: the fallback export renders a correct if
		// plainer file, which beats a download of compressed noise.
		log.Printf("api: job %s: input %s is named .gz but is not gzip: %v", job.ID, uri, err)
		rc.Close()
		return nil, noop, false
	}
	return gz, func() { gz.Close(); rc.Close() }, true
}

// exportMergedVCF writes the submitted VCF back with the annotations added.
//
// Reports false when there is no stored input to merge onto — a job old enough
// to have been swept, say — so the caller can fall back to rendering from rows
// rather than failing a download outright.
func (s *Server) exportMergedVCF(w http.ResponseWriter, r *http.Request, job queue.Chunk,
	cols []queue.Column, qy queue.ResultQuery) bool {

	// Built when the job finished, by the worker that already had the file
	// staged. The merge is the same either way; this is the copy that did not
	// have to parse and rewrite the whole submission to answer one download.
	if uri, ok, err := s.queue.ResultVCF(r.Context(), job.ID); err == nil && ok {
		rc, oErr := blob.Open(r.Context(), uri)
		if oErr == nil {
			defer rc.Close()
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if _, cErr := io.Copy(w, rc); cErr != nil {
				log.Printf("api: streaming stored result vcf for %s: %v", job.ID, cErr)
			}
			return true
		}
		// Gone or unreachable. Merging again is slower but still correct, so
		// fall through rather than fail a download over a missing shortcut.
		log.Printf("api: job %s: stored result vcf %s unreadable, merging again: %v",
			job.ID, uri, oErr)
	}

	src, closeSrc, ok := s.openJobInput(r, job)
	if !ok {
		return false
	}
	defer closeSrc()

	// The annotations, keyed by allele. Held in memory: they are already
	// materialized rows, and the alternative — a query per record — would be a
	// round trip per line of the file.
	qy.Limit, qy.Offset = 0, 0
	byAllele := vcfmerge.Annotations{}
	if err := s.queue.StreamResults(r.Context(), job.ID, qy, func(v queue.Variant) error {
		byAllele[vcfmerge.VariantKey(v.Chrom, v.Pos, v.Ref, v.Alt)] = v.Annotations
		return nil
	}); err != nil {
		return false
	}

	rd, err := vcf.NewVcfReader(src)
	if err != nil {
		return false
	}
	hdr, err := rd.Header()
	if err != nil {
		return false
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// Headers are already sent, so a failure here cannot become an error
	// response — it can only truncate. Logged and abandoned, which is what the
	// streaming export next door does for the same reason.
	if _, err := vcfmerge.Merge(rd, w, hdr, cols, byAllele); err != nil {
		log.Printf("api: merged vcf %s: %v", job.ID, err)
	}
	return true
}
