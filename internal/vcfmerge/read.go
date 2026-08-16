package vcfmerge

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
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
// A file without those lines falls back to using each INFO id as its own key.
//
// That is not a guess, it is the other writer's contract: varhub emits an
// annotation under the name the manifest gave it, so for a file it wrote the id
// *is* the key. The mapping lines exist for the case where that cannot hold —
// this service merging onto a submitted VCF, where an id may have been sanitised
// or suffixed to avoid colliding with a field the submitter already had.
//
// The distinction matters because there is one exception: cghts's builtins write
// fixed names of their own (CG_TSTV and the like) whatever the manifest calls
// them, and auto_id sets the record ID rather than an INFO field. Those come
// back under the name the file uses, which is at least the truth about the file.
func readColumnMap(hdr *vcf.VcfHeader) (keys map[string]string, flags map[string]bool) {
	keys, flags = map[string]string{}, map[string]bool{}
	for _, line := range hdr.OtherLines() {
		if id, key, ok := ParseColumnLine(line); ok {
			keys[id] = key
		}
	}
	if len(keys) == 0 {
		// No mapping: every declared INFO field is its own key.
		for _, id := range hdr.InfoIDs() {
			keys[id] = id
		}
	}
	for id := range keys {
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

// RowsFrom reads at most max variants out of a stored VCF file.
//
// Bounded because the caller wants the front of the result, not all of it: these
// rows are what a results table pages through, and the file they come from may
// hold a chromosome. Reading stops as soon as enough have been collected, so the
// cost is the cap rather than the file.
//
// max of 0 or less reads the lot, which is what an unbounded setting means.
func RowsFrom(path string, max int) ([]queue.Variant, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var src io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			return nil, fmt.Errorf("%s is named .gz but is not gzip: %w", path, gzErr)
		}
		defer gz.Close()
		src = gz
	}

	var out []queue.Variant
	err = Rows(src, func(v queue.Variant) error {
		out = append(out, v)
		if max > 0 && len(out) >= max {
			return errEnough
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnough) {
		return nil, err
	}
	return out, nil
}

// errEnough stops the walk once the cap is reached. A sentinel rather than a
// flag checked per record, so the reader stops rather than skipping the rest of
// a file it has no use for.
var errEnough = errors.New("enough rows")
