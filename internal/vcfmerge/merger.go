package vcfmerge

import (
	"fmt"
	"io"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Annotations are a job's results keyed by VariantKey.
type Annotations map[string]map[string]any

// Merger writes a submitted VCF back with annotation INFO fields added.
//
// Header and records are separable, and that is the whole design. A single job
// writes both. A chunk of a split VCF writes records only, so the collect step
// can concatenate every chunk under one header — twenty-six headers interleaved
// through a file is not a VCF, and stripping them afterwards means trusting that
// all of them were identical.
//
// The INFO ids are computed once here from the submitter's own header, so the
// same annotation lands under the same id in every chunk. That holds because
// the splitter copies the header into each chunk; if it ever stopped doing so,
// two chunks could name the same annotation differently and the concatenation
// would be a file whose header describes only part of it.
type Merger struct {
	hdr  *vcf.VcfHeader
	cols []queue.Column
	ids  map[string]string // annotation key -> INFO id
	flag map[string]bool   // annotation key -> is a Flag
}

// New prepares a merge against a submitted file's header.
//
// The header is modified in place with the added INFO definitions, so the
// caller should not reuse it for anything expecting the original.
func New(hdr *vcf.VcfHeader, cols []queue.Column) *Merger {
	// Which ids the submitter's own header already claims. Their definitions
	// stay; ours are added beside them, because overwriting theirs would
	// silently replace data they sent with data we computed, under a name they
	// chose.
	taken := map[string]bool{}
	for _, id := range hdr.InfoIDs() {
		taken[id] = true
	}
	m := &Merger{
		hdr:  hdr,
		cols: cols,
		ids:  make(map[string]string, len(cols)),
		flag: make(map[string]bool, len(cols)),
	}
	for _, c := range cols {
		id := uniqueInfoID(c.Key, taken)
		m.ids[c.Key] = id
		typ := InfoType(c.Type)
		m.flag[c.Key] = typ == "Flag"
		number := "1"
		if typ == "Flag" {
			number = "0"
		}
		hdr.AddInfo(&vcf.AnnotationDef{
			IsInfo: true, ID: id, Number: number, Type: typ,
			Description: HeaderDescription(c),
			Source:      "VariantHub",
		})
	}
	return m
}

// WriteHeader writes the submitted header with the annotation definitions added.
//
// Called once for a whole file, and once by the collect step for a set of
// chunks — never by a chunk itself.
func (m *Merger) WriteHeader(w io.Writer) error {
	out := vcf.NewVcfWriter(w)
	if err := out.WriteHeader(m.hdr); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	// Close flushes the buffered writer. It does not close w, which this does
	// not own.
	return out.Close()
}

// WriteRecords annotates every record from rd and writes it to w, without a
// header.
//
// Returns how many records were written, which is what a chunk reports so the
// collect step can check that the parts add up to the whole.
func (m *Merger) WriteRecords(rd *vcf.VcfReader, w io.Writer, ann Annotations) (int, error) {
	out := vcf.NewVcfWriter(w)
	n := 0
	for {
		rec, err := rd.NextRecord()
		if err != nil {
			break // including EOF
		}
		annotateRecord(rec, ann, m.cols, m.ids, m.flag)
		if err := out.WriteRecord(rec); err != nil {
			return n, fmt.Errorf("write record %d: %w", n+1, err)
		}
		n++
	}
	if err := out.Close(); err != nil {
		return n, fmt.Errorf("flush: %w", err)
	}
	return n, nil
}

// Merge is the whole file at once: header, then every record.
//
// What a single unsplit job produces, and what the API serves directly.
func Merge(rd *vcf.VcfReader, w io.Writer, hdr *vcf.VcfHeader, cols []queue.Column,
	ann Annotations) (int, error) {

	m := New(hdr, cols)
	if err := m.WriteHeader(w); err != nil {
		return 0, err
	}
	return m.WriteRecords(rd, w, ann)
}
