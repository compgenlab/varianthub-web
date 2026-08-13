package api

import (
	"compress/gzip"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// Where an export's rows come from.
//
// The job's stored result VCF, which the worker built when the job finished, is
// the answer. Every format is a conversion of that one object rather than a
// second rendering from the copy of the same data in chunk_variant — two
// renderings of one answer is two things that can disagree, and the one that
// drifts is whichever is exercised least.
//
// The rows still serve two things the file cannot. A search is a question about
// the whole set, and a sort is an order the file is not in; the file has no
// index and answering either from it would mean reading all of it into memory,
// which is what streaming exists to avoid. Postgres already holds the rows
// indexed, so it answers those. This is not a fallback in the apologetic sense —
// it is the query engine doing the query.

// resultURI is where a job's stored answer lives.
//
// False is an ordinary answer, not a failure: a job whose storage was swept, or
// one whose worker could not build the file.
func (s *Server) resultURI(r *http.Request, job queue.Job) (string, bool) {
	uri, ok, err := s.queue.ResultVCF(r.Context(), job.ID)
	if err != nil {
		log.Printf("api: job %s: locate result vcf: %v", job.ID, err)
		return "", false
	}
	return uri, ok
}

// openStoredResult opens a job's stored answer, decompressed.
//
// This is the reading path — what the tab, csv and json conversions consume.
// Serving the VCF itself does not come through here: it hands the object over
// as it is, or a link to it. See exportVCFResult.
func (s *Server) openStoredResult(r *http.Request, job queue.Job) (io.ReadCloser, bool) {
	uri, ok := s.resultURI(r, job)
	if !ok {
		return nil, false
	}
	rc, err := blob.Open(r.Context(), uri)
	if err != nil {
		log.Printf("api: job %s: open result %s: %v", job.ID, uri, err)
		return nil, false
	}
	// Told by the name, not sniffed from the bytes — the same rule the stored
	// input follows. This is where a split job's download used to go wrong: the
	// collect wrote gzip, the export copied it out verbatim as text/plain, and
	// the file called .vcf was compressed.
	if !queue.Compressed(uri) {
		return rc, true
	}
	gz, err := gzip.NewReader(rc)
	if err != nil {
		log.Printf("api: job %s: result %s is named .gz but is not gzip: %v", job.ID, uri, err)
		rc.Close()
		return nil, false
	}
	return bothClosed{Reader: gz, closers: []io.Closer{gz, rc}}, true
}

// bothClosed closes the decompressor and the object under it. Closing only the
// outer one leaks the connection the object is being read over, which on a busy
// server is a pool that stops handing anything out.
type bothClosed struct {
	io.Reader
	closers []io.Closer
}

func (b bothClosed) Close() error {
	var first error
	for _, c := range b.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// rowSource returns the variant stream an export should read, and a function to
// release it.
func (s *Server) rowSource(r *http.Request, job queue.Job, qy queue.ResultQuery) (vcfmerge.Stream, func()) {
	fromRows := func(fn func(queue.Variant) error) error {
		return s.queue.StreamResults(r.Context(), job.ID, qy, fn)
	}
	if !fileCanAnswer(qy) {
		return fromRows, func() {}
	}
	rc, ok := s.openStoredResult(r, job)
	if !ok {
		return fromRows, func() {}
	}
	return func(fn func(queue.Variant) error) error {
		return vcfmerge.Rows(rc, fn)
	}, func() { rc.Close() }
}

// fileCanAnswer reports whether the stored file, read start to end, is the set
// this query asked for.
//
// It is when nothing is being filtered and the order asked for is the order the
// file is in: input order, which is what an unspecified sort means and what
// "idx" names. Anything else — a search, another column, or the same order
// reversed — is a question about rows, and the rows answer it.
func fileCanAnswer(qy queue.ResultQuery) bool {
	if strings.TrimSpace(qy.Search) != "" || qy.Desc {
		return false
	}
	return qy.Sort == "" || qy.Sort == "idx"
}
