package cacherunner

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/compgenlab/cghts/htsio/bgzf"
	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// Reading and writing the files this decorator sits between.
//
// It has an input VCF and owes an output VCF, and in between it asks the engine
// about a subset of what it was given. All three are the same file in different
// states, which is what makes the arithmetic work: the subset is the input with
// records removed, so it comes back in the same order, and the answer is the
// input with annotations added.

// openVCF opens a VCF for reading, decompressed if its name says it is.
//
// Told by the name rather than sniffing the bytes, the same rule every other
// reader in this service follows: the process that stored the file recorded what
// it was, and a second opinion is a chance to disagree.
func openVCF(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s is named .gz but is not gzip: %w", path, err)
	}
	return readCloser{Reader: gz, closers: []io.Closer{gz, f}}, nil
}

type readCloser struct {
	io.Reader
	closers []io.Closer
}

func (r readCloser) Close() error {
	var first error
	for _, c := range r.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// writeSubset copies the records of in whose alleles are wanted, header and all.
//
// A filtered copy of the input rather than a synthesized sites-only file, and
// the difference is not cosmetic. The engine's VCF path preserves samples so
// that sample-derived annotations — dosage, VAF, strand bias — can be computed,
// and it computes them from the samples in the file it is given. Handing it
// bare loci would produce those values from no data at all, silently, for the
// annotations that are least cacheable and so most likely to be in a reduced
// run.
//
// Keeping the records also makes the subset a subsequence of the input in the
// same order, which is what lets the answer be assembled without sorting
// anything.
//
// A multi-allelic record is kept when any of its alleles is wanted. Splitting it
// would change what the engine sees, and per-allele filtering is what the cache
// lookup already did.
func writeSubset(inPath, outPath string, want map[string]bool) (int, error) {
	src, err := openVCF(inPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	out, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	w := bufio.NewWriterSize(out, 1<<20)
	sc := bufio.NewScanner(src)
	// A cohort record grows with its sample count; the 64 KB default would
	// report a long one as end of input and silently drop the rest of the file.
	sc.Buffer(make([]byte, 0, 256*1024), maxVCFLine)

	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if line[0] == '#' {
			if _, err := w.WriteString(line + "\n"); err != nil {
				return 0, err
			}
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		pos, convErr := strconv.ParseInt(f[1], 10, 64)
		if convErr != nil {
			continue
		}
		keep := false
		for _, alt := range strings.Split(f[4], ",") {
			l := anncache.Locus{
				Chrom: f[0], Pos: pos,
				Ref: strings.ToUpper(f[3]), Alt: strings.ToUpper(strings.TrimSpace(alt)),
			}
			if want[l.Key()] {
				keep = true
				break
			}
		}
		if !keep {
			continue
		}
		if _, err := w.WriteString(line + "\n"); err != nil {
			return 0, err
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", inPath, err)
	}
	return n, w.Flush()
}

// valuesFrom reads an annotated VCF back as locus key -> name -> value.
//
// What the engine just computed, in the form merge wants it. The keys are the
// INFO ids the engine wrote, which are the annotation names the manifest gave
// them — see vcfmerge.Rows for the one exception, and for why a file this
// service wrote carries an explicit mapping instead.
func valuesFrom(path string) (map[string]map[string]any, error) {
	src, err := openVCF(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	out := map[string]map[string]any{}
	err = vcfmerge.Rows(src, func(v queue.Variant) error {
		l := anncache.Locus{Chrom: v.Chrom, Pos: v.Pos, Ref: v.Ref, Alt: v.Alt}
		key := l.Key()
		if have, ok := out[key]; ok {
			for name, val := range v.Annotations {
				have[name] = val
			}
			return nil
		}
		out[key] = v.Annotations
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// writeAnswer writes the input back with every annotation set on it.
//
// The whole answer, cached and computed alike, assembled onto the file the
// caller submitted — so the samples, the INFO they sent and everything else
// survive exactly as they would have if the engine had seen the lot.
//
// BGZF, because this is the object the worker stores and every stored result is
// BGZF; see queue.ResultName.
func writeAnswer(inPath, outPath string, cols []queue.Column, ann vcfmerge.Annotations) error {
	src, err := openVCF(inPath)
	if err != nil {
		return err
	}
	defer src.Close()

	rd, err := vcf.NewVcfReader(src)
	if err != nil {
		return err
	}
	hdr, err := rd.Header()
	if err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := bgzf.NewWriter(out)
	if _, err := vcfmerge.Merge(rd, zw, hdr, cols, ann); err != nil {
		zw.Close()
		return err
	}
	// Closing writes the final block and the BGZF terminator. Skipped, the file
	// is truncated in a way only an htslib reader notices.
	return zw.Close()
}
