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
// So the join is a byte concatenation, which works because gzip members
// concatenate: several gzip streams laid end to end are one valid gzip stream,
// and every reader handles it. The first chunk keeps its header and the rest are
// written without one, so the result has exactly one header — and the whole
// thing streams from storage to storage without touching a disk.
package fanout

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/blob"
)

// StripHeader copies a VCF from r to w without its header lines.
//
// For every chunk but the first. Their headers are identical — each chunk was
// cut from one file and annotated with one column set — so keeping them would
// put the same header two dozen times through the middle of the output, where
// no reader expects one.
//
// Input and output are both gzip. The output is a plain gzip member rather than
// BGZF: the concatenation is not block-indexable whatever is done here, since
// its members come from separate compressors, and a reader only needs it to be
// gzip.
func StripHeader(r io.Reader, w io.Writer) (records int, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("read chunk: %w", err)
	}
	defer gz.Close()

	zw := gzip.NewWriter(w)
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
	for _, uri := range chunks {
		rc, err := blob.Open(ctx, uri)
		if err != nil {
			return fmt.Errorf("open chunk %s: %w", uri, err)
		}
		readers = append(readers, rc)
		closers = append(closers, rc)
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

// ChunkName is where a chunk's annotated output is stored within a batch.
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
