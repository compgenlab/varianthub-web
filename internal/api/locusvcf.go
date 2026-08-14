package api

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/compgenlab/cghts/htsio/bgzf"
)

// Turning a pasted locus list into the VCF everything downstream reads.
//
// A submission arrives one of two ways — a file, or a list of chrom:pos:ref:alt
// typed into a box — and until now those stayed two shapes all the way through:
// the file went to storage and the list travelled inline as a job's body, so
// every consumer had to know which kind it was holding. Converting at the door
// makes a job's stored input always a VCF, which is what lets the worker, the
// cache and the engine each stop asking.
//
// The order is the order it was pasted. Sorting into coordinate order was
// considered and rejected: it hands somebody back their variants in an order
// they did not ask for, and nothing needs it — see the note on indexing below.

// writeLocusVCF writes loci as a sites-only VCF, BGZF-compressed.
//
// Sites-only because a locus list has nothing else: no ID, no QUAL, no FILTER,
// no samples. Those are written missing rather than withheld, so the file is a
// VCF that any tool will read rather than a private format that happens to
// resemble one.
//
// The records keep the order they were given. That leaves the file unsorted when
// the person typed it unsorted, so it cannot be tabix-indexed — which costs
// nothing here: a pasted list is tens or hundreds of variants, and the file that
// is worth indexing is a submitted VCF, which arrives sorted already.
func writeLocusVCF(w io.Writer, loci []string) (int, error) {
	zw := bgzf.NewWriter(w)
	n := 0

	if _, err := io.WriteString(zw, "##fileformat=VCFv4.2\n"+
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"); err != nil {
		zw.Close()
		return 0, err
	}
	for _, l := range loci {
		chrom, pos, ref, alt, err := parseLocus(l)
		if err != nil {
			zw.Close()
			return 0, err
		}
		if _, err := fmt.Fprintf(zw, "%s\t%d\t.\t%s\t%s\t.\t.\t.\n",
			chrom, pos, ref, alt); err != nil {
			zw.Close()
			return 0, err
		}
		n++
	}
	// Closing writes the final block and the BGZF terminator; without it the
	// object uploads cleanly and is truncated on the way back.
	if err := zw.Close(); err != nil {
		return 0, err
	}
	return n, nil
}

// parseLocus splits chrom:pos:ref:alt.
//
// Rejecting rather than skipping a line that does not parse. A skipped variant
// is one the caller asked about and will not get an answer for, and they would
// have no way to know which — the count would simply be lower than what they
// pasted.
func parseLocus(s string) (chrom string, pos int64, ref, alt string, err error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 4 {
		return "", 0, "", "", fmt.Errorf(
			"%q is not chrom:pos:ref:alt", truncate(s, 40))
	}
	p, convErr := strconv.ParseInt(parts[1], 10, 64)
	if convErr != nil || p < 1 {
		return "", 0, "", "", fmt.Errorf("%q has no position", truncate(s, 40))
	}
	if parts[0] == "" || parts[2] == "" || parts[3] == "" {
		return "", 0, "", "", fmt.Errorf("%q is missing a field", truncate(s, 40))
	}
	// Upper-cased to match what the engine does with them, so the coordinates
	// echoed back are the ones that were asked about.
	return parts[0], p, strings.ToUpper(parts[2]), strings.ToUpper(parts[3]), nil
}

// truncate shortens a value for an error message.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
