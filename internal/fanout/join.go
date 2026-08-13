// Package fanout joins the pieces of a split VCF back into one file.
//
// The pieces are produced independently — one annotation job each — and have to
// become a single VCF at the end. The obvious way is to parse them and write
// them out again, which is what cgkit's vcf-concat does properly: it merges by
// contig and position, refuses overlaps, and unions the header definitions.
//
// This does not do that, on purpose. vcf-concat needs every piece on local disk
// at once, and ephemeral disk is the binding constraint here — a chromosome is
// hundreds of megabytes across dozens of chunks. The pieces also do not need
// merging: they come from one sorted file cut in order, so they are already
// disjoint and already in sequence.
//
// So the join is a byte concatenation, which works because BGZF members
// concatenate: BGZF is a multi-member gzip format whose blocks are each
// self-describing and independently compressed, so several BGZF streams laid end
// to end are one valid BGZF stream. This is what `bcftools concat --naive` does.
// The first chunk keeps its header and the rest are written without one, so the
// result has exactly one header — and the whole thing streams from storage to
// storage without touching a disk.
//
// BGZF and not plain gzip, which is what this did at first, on the reasoning
// that "the concatenation is not block-indexable whatever is done here, since
// its members come from separate compressors". That was wrong twice over. Block
// boundaries do not have to be agreed between compressors — each block stands
// alone, which is the entire point of the format — and the output is a VCF,
// which is a file somebody is going to run tabix over. Plain gzip is readable
// and unindexable, and the difference does not show up until a user tries to
// index a file this produced.
//
// The one thing concatenation does have to handle is the EOF block. Every BGZF
// stream ends with a fixed 28-byte empty block, and htslib stops reading at an
// empty block — so the marker from chunk 1 would truncate the joined file at
// chunk 1 for every htslib reader, while gzip -d happily decompressed the whole
// thing. See trimBGZFEOF.
package fanout

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/compgenlab/cghts/htsio/bgzf"

	"github.com/compgenlab/varianthub-web/internal/blob"
)

// StripHeader copies a VCF from r to w without its header lines.
//
// For every chunk but the first. Their headers are identical — each chunk was
// cut from one file and annotated with one column set — so keeping them would
// put the same header two dozen times through the middle of the output, where
// no reader expects one.
//
// Read as gzip and written as BGZF. Reading with the plain gzip reader is
// deliberate and costs nothing: BGZF is gzip, so this accepts either, and a
// chunk that arrived from some other tool is still readable.
func StripHeader(r io.Reader, w io.Writer) (records int, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("read chunk: %w", err)
	}
	defer gz.Close()

	zw := bgzf.NewWriter(w)
	out := bufio.NewWriterSize(zw, 1<<20)

	sc := bufio.NewScanner(gz)
	// A cohort VCF's line grows with its sample count. The scanner's 64 KB
	// default would report a long record as end of input, silently truncating a
	// chunk to whatever fitted — and a short chunk joins into a short file that
	// looks perfectly valid.
	sc.Buffer(make([]byte, 0, 256*1024), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) > 0 && line[0] == '#' {
			continue
		}
		if len(line) == 0 {
			continue
		}
		if _, err := out.Write(line); err != nil {
			return records, err
		}
		if err := out.WriteByte('\n'); err != nil {
			return records, err
		}
		records++
	}
	if err := sc.Err(); err != nil {
		return records, fmt.Errorf("read chunk: %w", err)
	}
	if err := out.Flush(); err != nil {
		return records, err
	}
	return records, zw.Close()
}

// maxLine bounds one record. 8 MB covers tens of thousands of samples and still
// refuses a file that is not line-oriented at all.
const maxLine = 8 << 20

// Join concatenates stored chunks into one object, in the order given.
//
// Order is the caller's to get right and it is not checked here: these are
// slices of a coordinate-sorted file, so the sequence they were cut in is the
// only one that produces a sorted result. Sorting them here would mean parsing
// them, which is the cost this exists to avoid.
//
// Nothing is held: each chunk is read from storage and written to the
// destination as it goes, so the disk cost is zero and the memory cost is a
// buffer, whatever the file's size.
func Join(ctx context.Context, chunks []string, dest string) error {
	if len(chunks) == 0 {
		return fmt.Errorf("nothing to join")
	}

	readers := make([]io.Reader, 0, len(chunks))
	closers := make([]io.Closer, 0, len(chunks))
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for i, uri := range chunks {
		rc, err := blob.Open(ctx, uri)
		if err != nil {
			return fmt.Errorf("open chunk %s: %w", uri, err)
		}
		closers = append(closers, rc)
		// Every chunk but the last gives up its EOF block, so the joined file
		// has exactly one and it is at the end. The last keeps its own, which
		// is why nothing has to append one here.
		if i == len(chunks)-1 {
			readers = append(readers, rc)
			continue
		}
		readers = append(readers, trimBGZFEOF(rc, uri))
	}

	// Opened up front rather than lazily so a missing chunk fails before
	// anything is written. A join that discovers the gap halfway has already
	// uploaded a prefix of the answer, and a truncated VCF reads as a complete
	// one that happens to stop early.
	if err := blob.PutReader(ctx, dest, io.MultiReader(readers...)); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

// ChunkName is where a chunk's annotated output is stored within a job.
//
// Numbered from 1 and zero-padded, matching what cgkit vcf-split produces and
// what vcf-concat --chunks looks for. Keeping the same shape means the series
// can be handed to those tools unchanged when the general case is wanted —
// overlapping records, or chunks that need a real merge.
func ChunkName(n int) string {
	return fmt.Sprintf("chunk.%04d.vcf.gz", n)
}

// SplitBase is the local name vcf-split is pointed at. Its chunks land beside
// it as BASE.1.vcf.gz, BASE.2.vcf.gz and so on.
func SplitBase(dir string) string { return strings.TrimRight(dir, "/") + "/part" }

// bgzfEOF is the 28-byte empty block that terminates a BGZF stream.
//
// Fixed by the SAM/BAM specification, so it is a constant rather than something
// to compute. Copied rather than imported because cghts keeps its own
// unexported; the value cannot drift, since a different one would not be a BGZF
// EOF block.
var bgzfEOF = []byte{
	0x1f, 0x8b, 0x08, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff,
	0x06, 0x00, 0x42, 0x43, 0x02, 0x00, 0x1b, 0x00, 0x03, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// errNoEOFBlock reports a chunk that did not end the way a BGZF stream must.
var errNoEOFBlock = errors.New("chunk does not end with a BGZF EOF block")

// trimBGZFEOF returns r with its trailing BGZF EOF block withheld.
//
// Concatenating BGZF streams is valid, with one exception: the empty block each
// one ends with. htslib treats an empty block as end of stream, so the marker
// closing chunk 1 would end the joined file there for tabix, bcftools and
// pysam — while gzip -d decompressed all of it and reported nothing wrong. A
// truncated VCF reads exactly like a complete one that happens to stop early,
// which is the failure this whole package is arranged to avoid.
//
// Refuses a chunk not ending in the marker rather than passing it through. That
// would mean the piece was written by something other than the BGZF writer, and
// silently concatenating it produces a file whose tail is unreadable at a point
// nothing records.
func trimBGZFEOF(r io.Reader, name string) io.Reader {
	return &eofTrimmer{src: r, name: name}
}

type eofTrimmer struct {
	src     io.Reader
	name    string
	pending []byte // withheld bytes; the last len(bgzfEOF) are never emitted
	scratch []byte
	atEOF   bool
}

func (t *eofTrimmer) Read(p []byte) (int, error) {
	for {
		// Anything beyond the reserve is known not to be the final block.
		if n := len(t.pending) - len(bgzfEOF); n > 0 {
			if n > len(p) {
				n = len(p)
			}
			copy(p, t.pending[:n])
			t.pending = append(t.pending[:0], t.pending[n:]...)
			return n, nil
		}
		if t.atEOF {
			if !bytes.Equal(t.pending, bgzfEOF) {
				return 0, fmt.Errorf("%s: %w", t.name, errNoEOFBlock)
			}
			return 0, io.EOF
		}
		if t.scratch == nil {
			t.scratch = make([]byte, 64*1024)
		}
		n, err := t.src.Read(t.scratch)
		t.pending = append(t.pending, t.scratch[:n]...)
		switch {
		case err == io.EOF:
			t.atEOF = true
		case err != nil:
			return 0, err
		}
	}
}
