package fanout

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultCgkitBin is the tool that splits a VCF. On PATH in the worker image,
// which already installs it for the build recipes.
const DefaultCgkitBin = "cgkit"

// Split cuts a local VCF into chunks of n records and returns their paths, in
// order.
//
// Shelled out to cgkit rather than reimplemented. vcf-split already handles the
// parts that are easy to get subtly wrong — the header copied into every chunk,
// a consistent numbering that vcf-concat can walk, and a failed run leaving no
// partial series — and a second implementation of that would be a second set of
// those bugs.
//
// Local in and local out, because that is what the tool does: cgkit reads s3://
// but refuses to write it. The caller stages the input and uploads the chunks.
func Split(ctx context.Context, bin, inPath, outBase string, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("chunk size must be positive, got %d", n)
	}
	if bin == "" {
		bin = DefaultCgkitBin
	}
	if err := os.MkdirAll(filepath.Dir(outBase), 0o755); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, bin, "vcf-split",
		"--out", outBase, "--num", strconv.Itoa(n), "--force", inPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The tool's own message, which names the line it objected to. Wrapping
		// it away would leave "exit status 1" as the whole explanation of why
		// somebody's submission failed.
		return nil, fmt.Errorf("vcf-split: %w: %s", err, strings.TrimSpace(string(out)))
	}

	chunks := existingChunks(outBase)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("vcf-split produced no chunks from %s", inPath)
	}
	return chunks, nil
}

// existingChunks lists a series on disk, stopping at the first gap.
//
// The same rule vcf-concat --chunks reads by, and deliberately so: a series with
// a hole in it must look the same length to whoever produced it and whoever
// consumes it. Counting the files instead would find a chunk beyond a gap and
// join a file that silently skips a range of the genome.
func existingChunks(base string) []string {
	var out []string
	for n := 1; ; n++ {
		p := fmt.Sprintf("%s.%d.vcf.gz", base, n)
		if _, err := os.Stat(p); err != nil {
			return out
		}
		out = append(out, p)
	}
}
