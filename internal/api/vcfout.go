package api

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// VCF output for any job, rendered from the stored rows.
//
// Available whatever the input was, not only for a submitted VCF. A locus list
// annotated here is still a set of variants, and a VCF is what the next tool in
// somebody's pipeline reads — so the fields a locus list cannot supply are
// written as missing rather than withheld as a reason not to offer the format.
// ID, QUAL and FILTER are "." and there are no samples; CHROM, POS, REF and ALT
// are real, and the annotations are the INFO.
//
// A job submitted as a VCF gets this too. Preserving the submitted file's own
// headers, INFO and sample columns is a separate and larger thing — it needs the
// input kept and the annotations merged back onto it — and this does not pretend
// to be that.

// exportVCF streams the job's results as a VCF.
func (s *Server) exportVCF(w http.ResponseWriter, r *http.Request, job queue.Job,
	cols []queue.Column, qy queue.ResultQuery) {

	// Coordinate order, whatever was asked for. A VCF sorted by CADD is not a
	// VCF anything can index, and it would look perfectly fine until someone ran
	// tabix on it. The search filter is still honoured — that changes which
	// records appear, not their order.
	qy.Sort, qy.Desc = "locus", false

	flag := map[string]bool{}
	ids := make(map[string]string, len(cols))
	used := map[string]int{}
	for _, c := range cols {
		id := vcfmerge.InfoID(c.Key)
		// Two keys can sanitise to the same ID, and silently merging them would
		// attribute one source's values to another.
		if n, clash := used[id]; clash {
			used[id] = n + 1
			id = fmt.Sprintf("%s_%d", id, n+1)
		} else {
			used[id] = 1
		}
		ids[c.Key] = id
		flag[c.Key] = vcfmerge.InfoType(c.Type) == "Flag"
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, "##fileformat=VCFv4.2\n")
	fmt.Fprintf(w, "##source=VariantHub %s\n", vcfmerge.Escape(s.cfg.Version))
	fmt.Fprintf(w, "##varianthub_job=%s\n", vcfmerge.Escape(job.ID))
	if job.Snapshot != "" {
		fmt.Fprintf(w, "##varianthub_snapshot=%s\n", vcfmerge.Escape(job.Snapshot))
	}
	for _, c := range cols {
		number := "1"
		typ := vcfmerge.InfoType(c.Type)
		if typ == "Flag" {
			number = "0"
		}
		fmt.Fprintf(w, "##INFO=<ID=%s,Number=%s,Type=%s,Description=\"%s\">\n",
			ids[c.Key], number, typ, vcfmerge.HeaderDescription(c))
	}
	fmt.Fprint(w, "#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n")

	err := s.queue.StreamResults(r.Context(), job.ID, qy, func(v queue.Variant) error {
		// Sorted so a record's INFO does not depend on Go's map iteration order,
		// which would make two identical runs differ byte for byte.
		keys := make([]string, 0, len(v.Annotations))
		for k := range v.Annotations {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		info := make([]string, 0, len(keys))
		for _, k := range keys {
			id, known := ids[k]
			if !known {
				id = vcfmerge.InfoID(k)
			}
			val, ok := vcfmerge.InfoValue(v.Annotations[k])
			if !ok {
				continue
			}
			if val == "" || flag[k] {
				info = append(info, id) // a Flag is its own presence
				continue
			}
			info = append(info, id+"="+val)
		}
		field := strings.Join(info, ";")
		if field == "" {
			field = "." // an empty INFO column is not valid; missing is "."
		}
		alt := v.Alt
		if alt == "" {
			alt = "."
		}
		ref := v.Ref
		if ref == "" {
			ref = "."
		}
		_, wErr := fmt.Fprintf(w, "%s\t%d\t.\t%s\t%s\t.\t.\t%s\n",
			v.Chrom, v.Pos, ref, alt, field)
		return wErr
	})
	if err != nil {
		// The header is already on the wire, so this cannot become a clean
		// error response. Log it; the client sees a short file.
		log.Printf("api: vcf export %s: %v", job.ID, err)
	}
}
