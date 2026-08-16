package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

// A pasted locus list becomes a VCF, in the order it was pasted.
//
// Sorting into coordinate order was considered and rejected: it hands somebody
// back their variants in an order they did not ask for. Nothing needs it — the
// cache merge walks the input and a subsequence of the input, which agree
// whatever order that is.
func TestALocusListBecomesAVCFInPasteOrder(t *testing.T) {
	var buf bytes.Buffer
	n, err := writeLocusVCF(&buf, []string{
		"chr2:50:C:T", // deliberately not in coordinate order
		"chr1:900:T:C",
		"chr1:100:a:g", // lower case, as somebody would paste it
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("wrote %d records, want 3", n)
	}

	body := gunzip(t, buf.Bytes())
	var data []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.HasPrefix(line, "#") {
			data = append(data, line)
		}
	}
	if !strings.HasPrefix(body, "##fileformat=VCFv4.2") {
		t.Errorf("not a VCF:\n%s", body)
	}
	if len(data) != 3 {
		t.Fatalf("got %d records:\n%s", len(data), body)
	}
	for i, want := range []string{"chr2\t50\t", "chr1\t900\t", "chr1\t100\t"} {
		if !strings.HasPrefix(data[i], want) {
			t.Errorf("record %d is %q, want it to start %q — the order was changed",
				i, data[i], want)
		}
	}
	// Alleles are upper-cased, matching what the engine does with them, so the
	// coordinates echoed back are the ones that were asked about.
	if !strings.Contains(data[2], "\tA\tG\t") {
		t.Errorf("alleles were not normalised: %q", data[2])
	}
	// Sites-only: the fields a locus list cannot supply are missing, not absent.
	if f := strings.Split(data[0], "\t"); len(f) != 8 || f[2] != "." || f[5] != "." || f[6] != "." {
		t.Errorf("record is not a well-formed sites-only line: %q", data[0])
	}
}

// The stored object is BGZF, like every other VCF this service writes.
func TestAStoredLocusListIsBGZF(t *testing.T) {
	var buf bytes.Buffer
	if _, err := writeLocusVCF(&buf, []string{"chr1:100:A:G"}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) < 18 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatal("not gzip at all")
	}
	if b[3]&0x04 == 0 || b[12] != 'B' || b[13] != 'C' {
		t.Error("the stored locus list is plain gzip, not BGZF")
	}
}

// A locus that does not parse fails the submission rather than being dropped.
//
// A skipped variant is one the caller asked about and will get no answer for,
// and they would have no way to tell which: the count would simply be lower
// than what they pasted.
func TestABadLocusIsRefusedNotSkipped(t *testing.T) {
	for _, bad := range []string{
		"chr1:100:A",   // too few fields
		"chr1:abc:A:G", // no position
		"chr1:0:A:G",   // positions are 1-based
		":100:A:G",     // no contig
		"chr1:100::G",  // no ref
	} {
		var buf bytes.Buffer
		if _, err := writeLocusVCF(&buf, []string{"chr1:100:A:G", bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
