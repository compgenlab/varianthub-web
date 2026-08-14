package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubVarhub writes a fake CLI that records its argv, and — if it was given a
// --loci-file — a copy of that file's contents, then emits one variant so the
// caller parses a result instead of failing on empty output.
//
// Returns the binary path and the two recording paths.
func stubVarhub(t *testing.T) (bin, argvPath, lociPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "varhub")
	argvPath = filepath.Join(dir, "argv")
	lociPath = filepath.Join(dir, "loci-seen")

	// Walks argv looking for --loci-file, then copies what it points at. Doing
	// the copy here rather than reading the runner's work dir from the test is
	// what makes this an assertion about the file the CLI was actually handed:
	// the runner deletes its scratch directory when the job ends.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + argvPath + "\n" +
		"prev=''\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = '--loci-file' ]; then cat \"$a\" > " + lociPath + "; fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		// The answer is a VCF at the -o path now, not JSON on stdout. gzip
		// rather than bgzip because the survey only needs to read it, and a
		// shell stub has no bgzip.
		"out=''\n" +
		"prev=''\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = '-o' ]; then out=\"$a\"; fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		"{ printf '##fileformat=VCFv4.2\\n'; " +
		"printf '#CHROM\\tPOS\\tID\\tREF\\tALT\\tQUAL\\tFILTER\\tINFO\\n'; " +
		"printf 'chr1\\t1\\t.\\tA\\tT\\t.\\t.\\t.\\n'; } | gzip > \"$out\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvPath, lociPath
}

// stubRunner pairs a stub CLI with the minimal home FixedHome insists on.
func stubRunner(t *testing.T, bin string) *ExecRunner {
	t.Helper()
	hdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hdir, "config.toml"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &ExecRunner{Bin: bin, Home: FixedHome(hdir), Timeout: 60 * time.Second}
}

// Loci reach the engine through a file, and every one of them arrives.
//
// Asserted on both sides: the loci are in the file, and they are *not* in argv.
// Only checking the file would still pass if the runner wrote the file and also
// appended the loci as arguments, which is the shape that reintroduces the
// ARG_MAX ceiling while looking correct.
func TestLociTravelInAFileAndNotInArgv(t *testing.T) {
	bin, argvPath, lociPath := stubVarhub(t)
	r := stubRunner(t, bin)

	body := "chr1:100:A:T chr2:200:C:G\nchr3:300:G:A"
	if _, err := r.Annotate(context.Background(), Request{
		Kind: KindLocus, Snapshot: "s", Selection: "all", Body: []byte(body),
		OutputPath: outPath(t),
	}); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	seen, err := os.ReadFile(lociPath)
	if err != nil {
		t.Fatalf("the CLI was never given a --loci-file: %v", err)
	}
	for _, want := range []string{"chr1:100:A:T", "chr2:200:C:G", "chr3:300:G:A"} {
		if !strings.Contains(string(seen), want) {
			t.Errorf("%s missing from the loci file:\n%s", want, seen)
		}
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(argv)), "\n") {
		if strings.HasPrefix(line, "chr") {
			t.Errorf("locus %q was also passed as an argument; the ARG_MAX ceiling is back:\n%s",
				line, argv)
		}
	}
}

// The point of the file: a batch far past what argv can hold now runs.
//
// 200k loci is ~2.6 MB of text, comfortably over the ~2 MB a single exec gets on
// Linux. As arguments this failed with "argument list too long" — a message that
// names neither the limit nor the input, and that no amount of reading the job's
// error would explain. How many variants a job may carry is now a number the
// server chooses.
func TestALociBatchTooLargeForArgvSucceeds(t *testing.T) {
	bin, _, lociPath := stubVarhub(t)
	r := stubRunner(t, bin)

	const n = 200_000
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "chr1:%d:A:T\n", i+1)
	}
	if b.Len() < 2<<20 {
		t.Fatalf("the fixture is only %d bytes; it has to exceed ARG_MAX to test anything", b.Len())
	}

	if _, err := r.Annotate(context.Background(), Request{
		Kind: KindLocus, Snapshot: "s", Selection: "all", Body: []byte(b.String()),
		OutputPath: outPath(t),
	}); err != nil {
		t.Fatalf("a %d-locus batch failed: %v", n, err)
	}

	seen, err := os.ReadFile(lociPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(seen))); got != n {
		t.Errorf("the CLI was handed %d loci, want %d", got, n)
	}
}

// A locus the file format would eat has to be refused, not dropped.
//
// The reader skips blank lines and treats "#" as a comment, so a locus of that
// shape would leave the job annotating fewer variants than were submitted with
// nothing anywhere saying so — the worst kind of wrong answer, because it looks
// like a complete one.
func TestALocusThatLooksLikeACommentIsRefused(t *testing.T) {
	bin, _, _ := stubVarhub(t)
	r := stubRunner(t, bin)

	_, err := r.Annotate(context.Background(), Request{
		Kind: KindLocus, Snapshot: "s", Selection: "all",
		Body: []byte("chr1:100:A:T #chr2:200:C:G"),
		OutputPath: outPath(t),
	})
	if err == nil {
		t.Fatal("a #-leading locus was accepted; it would have been silently dropped")
	}
	if !strings.Contains(err.Error(), "comment") {
		t.Errorf("the error should say why, got: %v", err)
	}
}
