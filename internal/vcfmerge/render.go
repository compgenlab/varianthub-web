package vcfmerge

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Rendering a VCF for variants that never came from a file.
//
// A locus list is still a set of variants, and a VCF is what the next tool in
// somebody's pipeline reads — so the fields a locus list cannot supply are
// written as missing rather than withheld as a reason not to offer the format.
// ID, QUAL and FILTER are "." and there are no samples; CHROM, POS, REF and ALT
// are real, and the annotations are the INFO.
//
// This is the counterpart of Merge, which annotates a file the submitter sent.
// Both exist because both inputs exist, and a job of either kind has to end up
// with the same thing stored under the same name: one object per job, holding
// its answer, whatever it was asked with.

// Meta is what a rendered file says about where it came from.
type Meta struct {
	Version  string // the server's, for ##source
	JobID    string
	Snapshot string
}

// Stream calls fn for each variant in turn. It is a function rather than a
// slice because the caller with the most rows — an export of a finished job —
// is streaming them out of Postgres and must not hold them all.
type Stream func(fn func(queue.Variant) error) error

// Render writes a sites-only VCF.
//
// Errors from the stream are returned; errors from w are too, but by then bytes
// are already written, so a caller that has committed a response can only log.
func Render(w io.Writer, meta Meta, cols []queue.Column, stream Stream) error {
	// No submitted header, so nothing has claimed an id yet.
	ids, flag := assignInfoIDs(cols, map[string]bool{})

	fmt.Fprint(w, "##fileformat=VCFv4.2\n")
	if meta.Version != "" {
		fmt.Fprintf(w, "##source=VariantHub %s\n", Escape(meta.Version))
	}
	if meta.JobID != "" {
		fmt.Fprintf(w, "##varianthub_job=%s\n", Escape(meta.JobID))
	}
	if meta.Snapshot != "" {
		fmt.Fprintf(w, "##varianthub_snapshot=%s\n", Escape(meta.Snapshot))
	}
	for _, c := range cols {
		fmt.Fprintf(w, "##INFO=<ID=%s,Number=%s,Type=%s,Description=\"%s\",Source=\"VariantHub\">\n",
			ids[c.Key], InfoNumber(c.Type), InfoType(c.Type), HeaderDescription(c))
		fmt.Fprintln(w, ColumnLine(ids[c.Key], c.Key))
	}
	fmt.Fprint(w, "#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n")

	return stream(func(v queue.Variant) error {
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
				id = InfoID(k)
			}
			val, ok := InfoValue(v.Annotations[k])
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
		_, err := fmt.Fprintf(w, "%s\t%d\t.\t%s\t%s\t.\t.\t%s\n",
			v.Chrom, v.Pos, ref, alt, field)
		return err
	})
}
