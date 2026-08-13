package api

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/queue"
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

// mergeInfoID is the INFO id an annotation is written under, avoiding a
// collision with one the submitter already uses.
//
// Overwriting theirs would silently replace data they sent with data we
// computed, and under a name they chose — the kind of loss that is invisible
// until somebody compares the file with what they submitted.
func mergeInfoID(key string, taken map[string]bool) string {
	id := vcfInfoID(key)
	if !taken[id] {
		taken[id] = true
		return id
	}
	for n := 2; ; n++ {
		alt := fmt.Sprintf("%s_%d", id, n)
		if !taken[alt] {
			taken[alt] = true
			return alt
		}
	}
}

// variantKey identifies a record's allele for looking up its annotations.
func variantKey(chrom string, pos int64, ref, alt string) string {
	return fmt.Sprintf("%s:%d:%s:%s", chrom, pos, ref, alt)
}

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
func (s *Server) openJobInput(r *http.Request, job queue.Job) (io.Reader, func(), bool) {
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
func (s *Server) exportMergedVCF(w http.ResponseWriter, r *http.Request, job queue.Job,
	cols []queue.Column, qy queue.ResultQuery) bool {

	src, closeSrc, ok := s.openJobInput(r, job)
	if !ok {
		return false
	}
	defer closeSrc()

	// The annotations, keyed by allele. Held in memory: they are already
	// materialized rows, and the alternative — a query per record — would be a
	// round trip per line of the file.
	qy.Limit, qy.Offset = 0, 0
	byAllele := map[string]map[string]any{}
	if err := s.queue.StreamResults(r.Context(), job.ID, qy, func(v queue.Variant) error {
		byAllele[variantKey(v.Chrom, v.Pos, v.Ref, v.Alt)] = v.Annotations
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

	// Which ids the submitter's own header already claims. Their definitions
	// stay; ours are added beside them.
	taken := map[string]bool{}
	for _, id := range hdr.InfoIDs() {
		taken[id] = true
	}
	ids := make(map[string]string, len(cols))
	flag := make(map[string]bool, len(cols))
	for _, c := range cols {
		id := mergeInfoID(c.Key, taken)
		ids[c.Key] = id
		typ := vcfType(c.Type)
		flag[c.Key] = typ == "Flag"
		number := "1"
		if typ == "Flag" {
			number = "0"
		}
		hdr.AddInfo(&vcf.AnnotationDef{
			IsInfo: true, ID: id, Number: number, Type: typ,
			Description: vcfHeaderDescription(c),
			Source:      "VariantHub",
		})
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	out := vcf.NewVcfWriter(w)
	if err := out.WriteHeader(hdr); err != nil {
		log.Printf("api: merged vcf %s: header: %v", job.ID, err)
		return true
	}
	for {
		rec, err := rd.NextRecord()
		if err != nil {
			break // including EOF
		}
		annotateRecord(rec, byAllele, cols, ids, flag)
		if err := out.WriteRecord(rec); err != nil {
			log.Printf("api: merged vcf %s: %v", job.ID, err)
			break
		}
	}
	// Close flushes the buffered writer. It does not close the response, which
	// this writer does not own.
	if err := out.Close(); err != nil {
		log.Printf("api: merged vcf %s: flush: %v", job.ID, err)
	}
	return true
}

// annotateRecord sets a record's annotation INFO fields from the results.
//
// A multi-allelic record is annotated per ALT and the values joined with
// commas, in ALT order, which is how VCF expresses a per-allele field. Writing
// only the first allele's value would attribute it to all of them.
func annotateRecord(rec *vcf.VcfRecord, byAllele map[string]map[string]any,
	cols []queue.Column, ids map[string]string, flag map[string]bool) {

	alts := rec.Alt()
	if len(alts) == 0 {
		return
	}
	perAlt := make([]map[string]any, 0, len(alts))
	for _, alt := range alts {
		perAlt = append(perAlt, byAllele[variantKey(rec.Chrom, int64(rec.Pos), rec.Ref, alt)])
	}

	info := rec.Info()
	changed := false
	// Column order, not map order. Ranging the id map would put a record's
	// INFO fields in a different sequence on every run, so the same job would
	// export bytes that never match twice.
	for _, c := range cols {
		key := c.Key
		id := ids[key]
		vals := make([]string, len(perAlt))
		any := false
		for i, anns := range perAlt {
			if anns == nil {
				continue
			}
			v, ok := vcfInfoValue(anns[key])
			if !ok {
				continue
			}
			any = true
			if flag[key] {
				vals[i] = ""
				continue
			}
			vals[i] = v
		}
		if !any {
			continue
		}
		if flag[key] {
			info.SetFlag(id)
			changed = true
			continue
		}
		// Missing values within a multi-allelic field are "." rather than
		// empty, or the comma-separated list would not line up with the ALTs.
		for i, v := range vals {
			if v == "" {
				vals[i] = "."
			}
		}
		info.Set(id, strings.Join(vals, ","))
		changed = true
	}
	// Attributes has no back-reference to its record, so a record is written
	// verbatim unless it is told it changed. Without this the annotations were
	// set and then discarded on the way out — the file came back byte-perfect
	// and useless.
	if changed {
		rec.MarkDirty()
	}
}
