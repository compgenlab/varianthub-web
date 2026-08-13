package fanout

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/bgzf"
)

// isBGZF reports whether b begins with a BGZF block rather than a plain gzip
// member.
//
// The difference is the FEXTRA flag and a "BC" subfield carrying the block size.
// Nothing else distinguishes them — a BGZF file decompresses perfectly with gzip
// — which is exactly why writing plain gzip here went unnoticed: every check
// short of trying to index the file passes.
func isBGZF(b []byte) bool {
	if len(b) < 18 || b[0] != 0x1f || b[1] != 0x8b || b[3]&0x04 == 0 {
		return false
	}
	return b[12] == 'B' && b[13] == 'C'
}

// countEOFBlocks says how many BGZF terminators the bytes contain.
func countEOFBlocks(b []byte) int { return bytes.Count(b, bgzfEOF) }

// A chunk is stored as BGZF, not as plain gzip.
//
// The output is a VCF, and a VCF this size is one somebody will run tabix over.
// Plain gzip is readable and unindexable, and the difference does not appear
// until a user tries to index a file this produced.
func TestAStoredChunkIsBGZF(t *testing.T) {
	dir := t.TempDir()
	uri, err := StoreChunkResult(context.Background(), dir, 0,
		strings.NewReader(chunk(100, 3)), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !isBGZF(raw) {
		t.Error("the stored chunk is plain gzip; it cannot be indexed")
	}
	if got := countEOFBlocks(raw); got != 1 {
		t.Errorf("the chunk carries %d EOF blocks, want exactly 1", got)
	}
	if !bytes.HasSuffix(raw, bgzfEOF) {
		t.Error("the chunk does not end with the EOF block; htslib reports an " +
			"unterminated file")
	}
}

// A joined file carries exactly one EOF block, at the end.
//
// This is the whole hazard of concatenating BGZF. Every stream ends with an
// empty block and htslib stops reading at one, so the marker closing chunk 1
// would end the joined file there for tabix, bcftools and pysam — while gzip -d
// decompressed all of it and reported nothing wrong. A file truncated this way
// reads exactly like a complete one that happens to stop early.
func TestAJoinedFileHasOneEOFBlockAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	var uris []string
	for i := range 3 {
		uri, err := StoreChunkResult(ctx, dir, i,
			strings.NewReader(chunk(100+i*100, 2)), nil)
		if err != nil {
			t.Fatal(err)
		}
		uris = append(uris, uri)
	}

	dest := filepath.Join(dir, "joined.vcf.gz")
	if err := Join(ctx, uris, dest); err != nil {
		t.Fatalf("join: %v", err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := countEOFBlocks(raw); got != 1 {
		t.Errorf("the joined file carries %d EOF blocks, want 1: every chunk but "+
			"the last must give up its own", got)
	}
	if !bytes.HasSuffix(raw, bgzfEOF) {
		t.Error("the joined file does not end with an EOF block")
	}
	if !isBGZF(raw) {
		t.Error("the joined file is not BGZF")
	}

	// The block count above is the assertion with teeth, and it is worth saying
	// why the read below is not.
	//
	// cghts's reader is lenient: it reads straight past an interior EOF block and
	// returns everything, so this passes trimmed or untrimmed (verified). htslib
	// is not — bgzf_read breaks out of its loop on a zero-length block, which is
	// why bcftools concat --naive and samtools cat strip the markers instead of
	// concatenating whole files. Since tabix, bcftools and pysam are the
	// consumers that matter for a VCF, the byte-level check is the one standing
	// in for them until an htslib binary is available to test against.
	body, err := io.ReadAll(bgzf.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("reading the joined file as BGZF: %v", err)
	}
	records := 0
	for _, line := range strings.Split(string(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			records++
		}
	}
	if records != 6 {
		t.Errorf("read %d records from the joined file, want 6 — it was truncated "+
			"at a chunk boundary", records)
	}
}

// A chunk that is not BGZF is refused rather than concatenated.
//
// It would mean the piece was written by something other than the BGZF writer,
// and joining it produces a file whose tail is unreadable from a point nothing
// records.
func TestJoinRefusesAChunkWithNoEOFBlock(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	good, err := StoreChunkResult(ctx, dir, 0, strings.NewReader(chunk(100, 2)), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Plain gzip: valid gzip, no EOF block, and indistinguishable from the real
	// thing to anything that only decompresses.
	bad := filepath.Join(dir, "plain.vcf.gz")
	if err := os.WriteFile(bad, truncatedEOF(t, chunk(200, 2)), 0o600); err != nil {
		t.Fatal(err)
	}

	err = Join(ctx, []string{bad, good}, filepath.Join(dir, "out.vcf.gz"))
	if err == nil {
		t.Fatal("a chunk with no EOF block was joined without complaint")
	}
	if !strings.Contains(err.Error(), "EOF block") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// truncatedEOF returns a BGZF stream with its terminator removed, which is what
// a piece written by a plain gzip writer looks like from the outside.
func truncatedEOF(t *testing.T, s string) []byte {
	t.Helper()
	raw := bgzipped(t, s)
	return raw[:len(raw)-len(bgzfEOF)]
}
