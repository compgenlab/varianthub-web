package vcfmerge

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Reading a stored result VCF back as rows.
//
// This is the direction that makes the file the primary result rather than one
// of several renderings of it. A job's answer is annotated once, by the worker
// that ran it, and stored; the tab, csv and json exports are conversions of that
// object rather than second renderings from a copy of the same data in
// Postgres. Two renderings of one answer is two things that can disagree, and
// the one that drifts is whichever is exercised least.
//
// Everything needed is in the file. The INFO definitions give the types, and the
// ##varianthub_column lines give the annotation key each id carries — so this
// needs neither the database nor the snapshot that produced the job, and a file
// somebody downloaded a month ago still converts.

// Rows calls fn for every annotated allele in a stored result VCF, in file
// order.
//
// One call per ALT, not per record: a multi-allelic record carries one value per
// allele and the rows this server has always served are per allele, so a
// two-allele record becomes two rows.
func Rows(r io.Reader, fn func(queue.Variant) error) error {
	rd, err := vcf.NewVcfReader(r)
	if err != nil {
		return fmt.Errorf("read result vcf: %w", err)
	}
	hdr, err := rd.Header()
	if err != nil {
		return fmt.Errorf("read result vcf header: %w", err)
	}
	keys, flags := readColumnMap(hdr)

	for {
		rec, err := rd.NextRecord()
		if err != nil {
			break // including EOF; cghts reports the end this way
		}
		alts := rec.Alt()
		if len(alts) == 0 {
			alts = []string{""}
		}
		// Decoded once per record and shared out per allele, so a record with
		// twenty annotations and three alleles splits twenty strings rather than
		// sixty.
		perAllele := make([]map[string]any, len(alts))
		for i := range perAllele {
			perAllele[i] = map[string]any{}
		}
		info := rec.Info()
		for id, key := range keys {
			val, present := info.Get(id)
			if !present {
				continue
			}
			if flags[id] {
				// A Flag is per record — VCF has no way to attach one to a single
				// ALT — so its presence is every allele's.
				for i := range perAllele {
					perAllele[i][key] = true
				}
				continue
			}
			spread(perAllele, key, val.String(), hdr, id)
		}

		for i, alt := range alts {
			if alt == "." {
				alt = ""
			}
			ann := perAllele[i]
			if len(ann) == 0 {
				ann = map[string]any{}
			}
			ref := rec.Ref
			if ref == "." {
				ref = ""
			}
			if err := fn(queue.Variant{
				Chrom: rec.Chrom, Pos: int64(rec.Pos), Ref: ref, Alt: alt,
				Annotations: ann,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// spread assigns one INFO field's value to each allele.
//
// The writer emits one comma-separated value per ALT (Number=A), so the usual
// case is a positional split. A single value for a multi-allelic record is
// accepted and given to every allele: that is what Number=1 means, and a file
// this did not write is entitled to use it.
func spread(perAllele []map[string]any, key, raw string, hdr *vcf.VcfHeader, id string) {

	parts := strings.Split(raw, ",")
	for i := range perAllele {
		var text string
		switch {
		case len(parts) == 1:
			text = parts[0]
		case i < len(parts):
			text = parts[i]
		default:
			continue // fewer values than alleles; the rest simply had none
		}
		// "." is how a per-allele field says this allele had no value, and it is
		// the reason a missing annotation reads as missing rather than as the
		// string ".".
		if text == "" || text == "." {
			continue
		}
		perAllele[i][key] = decode(hdr, id, text)
	}
}

// decode turns one INFO value into what the json export would have carried.
//
// Numbers become float64 because that is what a number round-tripped through
// JSON is, and these values are compared against — and exported beside — rows
// that came back out of Postgres as JSON. Returning an int here would make the
// same annotation a different Go type depending on which source served it.
func decode(hdr *vcf.VcfHeader, id, text string) any {
	def, ok := hdr.InfoDef(id)
	if ok {
		switch def.Type {
		case "Integer", "Float":
			if f, err := strconv.ParseFloat(text, 64); err == nil {
				return f
			}
			// Declared numeric and is not. The value is still what the file
			// says, so it is served as text rather than dropped — a source that
			// emits "0.5;NA" should show that, not a hole.
		}
	}
	return Unescape(text)
}

// readColumnMap recovers which INFO id carries which annotation key.
//
// From the ##varianthub_column lines the writer left, which is the only exact
// answer: an id is sanitised from its key and may be suffixed to avoid a
// collision with the submitter's own fields, so it cannot be reversed.
//
// A file with no such lines yields no annotations rather than a guess. That is
// the safe direction — an export with an empty column is visibly wrong, while
// one built by matching ids to keys by resemblance is confidently wrong, and
// attributing one source's values to another is the failure this whole mapping
// exists to prevent.
func readColumnMap(hdr *vcf.VcfHeader) (keys map[string]string, flags map[string]bool) {
	keys, flags = map[string]string{}, map[string]bool{}
	for _, line := range hdr.OtherLines() {
		id, key, ok := ParseColumnLine(line)
		if !ok {
			continue
		}
		keys[id] = key
		if def, found := hdr.InfoDef(id); found && def.Type == "Flag" {
			flags[id] = true
		}
	}
	return keys, flags
}

// ColumnKeys lists the annotation keys a stored result VCF carries, in header
// order, so a reader can describe the file without the database.
func ColumnKeys(hdr *vcf.VcfHeader) []string {
	var out []string
	for _, line := range hdr.OtherLines() {
		if _, key, ok := ParseColumnLine(line); ok {
			out = append(out, key)
		}
	}
	return out
}
