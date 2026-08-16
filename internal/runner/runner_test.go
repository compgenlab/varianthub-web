package runner

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testHome builds a minimal VARHUB_HOME with a builtins-only snapshot, so the
// runner can be exercised against the real CLI without downloading any data.
// Set VHW_TEST_VARHUB to a varhub binary to enable; otherwise these skip.
func testHome(t *testing.T) (bin, home string) {
	t.Helper()
	bin = os.Getenv("VHW_TEST_VARHUB")
	if bin == "" {
		t.Skip("VHW_TEST_VARHUB not set; skipping runner integration tests")
	}
	if _, err := exec.LookPath(bin); err != nil {
		if _, statErr := os.Stat(bin); statErr != nil {
			t.Skipf("varhub binary %q not usable: %v", bin, err)
		}
	}
	home = t.TempDir()

	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.toml", `
data_dir = "$VARHUB_HOME/data"
cache_dir = "$VARHUB_HOME/data/cache"
default_snapshot = "test"
annotations_dir = "./annotations"
`)
	// The snapshot's name comes from the filename; the manifest key for defaults is
	// default_annotations. Both are easy to get wrong by hand.
	write("annotations/snapshots/test.toml", `
title = "Runner test snapshot"
assembly = "GRCh38"
sources = ["builtins:1"]
default_annotations = ["auto_id", "tstv"]
`)
	write("annotations/sources/builtins/1/builtins-1.toml", `
[[sources]]
  type = "builtin"
  name = "builtins"
  version = "1"

  [[sources.annotations]]
    builtin = "auto_id"
    name = "auto_id"

  [[sources.annotations]]
    builtin = "tstv"
    name = "tstv"
`)
	return bin, home
}

// outPath is where a test's annotated VCF goes. Named .vcf.gz because that is
// how cghts's writer is told to emit BGZF, which is what the runner surveys and
// what storage keeps.
func outPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "result.vcf.gz")
}

func TestExecRunnerLocus(t *testing.T) {
	bin, home := testHome(t)
	r := &ExecRunner{Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second}

	res, err := r.Annotate(context.Background(), Request{
		Kind:       KindLocus,
		Snapshot:   "test",
		Selection:  "all",
		Body:       []byte("chr1:115256529:T:C"),
		OutputPath: outPath(t),
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if res.N != 1 {
		t.Errorf("N = %d, want 1", res.N)
	}

	// The engine wrote a VCF, and it is at the path the caller chose.
	if res.VCFPath == "" {
		t.Fatal("no output path came back")
	}
	body := readAnnotated(t, res.VCFPath)
	if !strings.HasPrefix(body, "##fileformat=VCF") {
		t.Fatalf("the output is not a VCF:\n%s", body)
	}
	var data []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.HasPrefix(line, "#") {
			data = append(data, line)
		}
	}
	if len(data) != 1 {
		t.Fatalf("got %d records:\n%s", len(data), body)
	}
	if !strings.HasPrefix(data[0], "chr1\t115256529\t") {
		t.Errorf("unexpected record: %q", data[0])
	}
	// Builtins come back under the names the manifest gave them.
	//
	// They did not until varianthub-cli honoured a.Name: the annotator wrote
	// CG_TSTV whatever the manifest said, so the column model said tstv while
	// the file said CG_TSTV and nothing joined them. Pinned here because this is
	// where the consequence lands — a column key that does not match the file is
	// an empty column in somebody's results table.
	f := strings.Split(data[0], "\t")
	if !strings.Contains(data[0], "tstv=TS") {
		t.Errorf("tstv did not come back under its manifest name: %q", data[0])
	}
	if strings.Contains(data[0], "CG_TSTV") {
		t.Errorf("the annotator's own fixed name leaked into the output: %q", data[0])
	}
	// auto_id is the exception, and deliberately so: a variant identifier belongs
	// in the ID column, which is where it goes. It is not an INFO field and has
	// no name to take.
	if f[2] != "1-115256529-T-C" {
		t.Errorf("auto_id should be the record ID, got %q", f[2])
	}
}

// readAnnotated returns an annotated VCF as text, decompressing it.
func readAnnotated(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the engine wrote no output: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("the output is not gzip: %v", err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExecRunnerVCF(t *testing.T) {
	bin, home := testHome(t)
	r := &ExecRunner{Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second}

	vcf := strings.Join([]string{
		"##fileformat=VCFv4.2",
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO",
		"chr1\t100\t.\tA\tG\t.\t.\t.",
		"chr1\t250\t.\tC\tT\t.\t.\t.",
		"",
	}, "\n")

	res, err := r.Annotate(context.Background(), Request{
		Kind: KindVCF, Snapshot: "test", Selection: "all", Body: []byte(vcf),
		OutputPath: outPath(t),
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if res.N != 2 {
		t.Errorf("N = %d, want 2", res.N)
	}
}

func TestFixedHomeRejectsMissing(t *testing.T) {
	if _, _, err := FixedHome("").Home(context.Background(), ""); err == nil {
		t.Error("empty FixedHome should error")
	}
	if _, _, err := FixedHome(t.TempDir()).Home(context.Background(), ""); err == nil {
		t.Error("a dir with no config.toml should error")
	}
}

func asExitError(err error, target **ExitError) bool {
	for err != nil {
		if e, ok := err.(*ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A locus beginning with "-" must reach the CLI as input, not be swallowed as a
// flag — the failure being guarded against is "flag provided but not defined",
// which is a confusing way to report a bad variant.
//
// Loci now travel in a file, which makes this structurally true rather than
// dependent on a "--" terminator being in the right place. The test stays
// because the property is what matters, not the mechanism: it would fail again
// the moment anything put user input back into argv.
func TestLeadingDashLocusIsNotAFlag(t *testing.T) {
	bin, home := testHome(t)
	r := &ExecRunner{Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second}

	_, err := r.Annotate(context.Background(), Request{
		Kind: KindLocus, Snapshot: "test", Selection: "all", Body: []byte("-oops"),
		OutputPath: outPath(t),
	})
	if err == nil {
		t.Fatal("expected an error for a malformed locus")
	}
	var ee *ExitError
	if !asExitError(err, &ee) {
		t.Fatalf("expected *ExitError (a parse failure), got %T: %v", err, err)
	}
	if strings.Contains(ee.Detail(), "flag provided but not defined") {
		t.Errorf("locus was parsed as a flag; the -- terminator is missing:\n%s", ee.Detail())
	}
}

// A NUL byte in argv makes execve fail with EINVAL, reported as an opaque
// "fork/exec: invalid argument". Reject it up front with a message that names
// the offending input.
func TestControlCharactersRejected(t *testing.T) {
	bin, home := testHome(t)
	r := &ExecRunner{Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second}

	for _, tc := range []struct{ name, body string }{
		{"nul", "chr1:100:A:G\x00evil"},
		{"newline", "chr1:100:A:\tG\x01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Annotate(context.Background(), Request{
				Kind: KindLocus, Snapshot: "test", Selection: "all", Body: []byte(tc.body),
				OutputPath: outPath(t),
			})
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), "control character") {
				t.Errorf("error should name the cause, got: %v", err)
			}
			if strings.Contains(err.Error(), "invalid argument") {
				t.Errorf("leaked the opaque execve error: %v", err)
			}
		})
	}
}

func TestSafeArgLength(t *testing.T) {
	if err := safeArg(strings.Repeat("a", maxArgLen+1)); err == nil {
		t.Error("over-long argument should be rejected")
	}
	if err := safeArg(strings.Repeat("a", maxArgLen)); err != nil {
		t.Errorf("argument at the limit should be accepted: %v", err)
	}
}

// A failing job must tell the user what to fix. The CLI writes its errors for
// humans; hiding them behind "annotation failed" leaves a misconfigured source
// undiagnosable. But server paths must not leak.
func TestCLIMessageSurfacedAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, home, want string
	}{
		{
			name:   "cli error is surfaced",
			stderr: "varhub: starting\nerror: genelist \"x:1\": needs gtf = \"name[:version]\"",
			want:   `genelist "x:1": needs gtf = "name[:version]"`,
		},
		{
			name:   "last error wins",
			stderr: "error: first\nerror: second",
			want:   "second",
		},
		{
			name:   "home is redacted",
			stderr: "error: config file /home/x/varhub-home-123/config.toml not found",
			home:   "/home/x/varhub-home-123",
			want:   "config file <config>/config.toml not found",
		},
		{
			// A configured storage path is exactly what an operator needs to see —
			// it is a path they set and can already read off the admin screen.
			// Withholding it (as an earlier version did) made a permission error
			// undiagnosable.
			name:   "configured path is kept",
			stderr: "error: gencode:48: mkdir /var/lib/varianthub/sources/gencode: permission denied",
			want:   "gencode:48: mkdir /var/lib/varianthub/sources/gencode: permission denied",
		},
		{
			name:   "no error line stays opaque",
			stderr: "panic: runtime error\ngoroutine 1 [running]:",
			want:   "",
		},
		{name: "empty stderr", stderr: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cliMessage(tc.stderr, tc.home); got != tc.want {
				t.Errorf("cliMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// Unrecognised output used to be withheld entirely, on the theory that a
// failure mode we do not understand might print something better kept quiet.
// The cost was that the failures hardest to diagnose were the ones reported as
// a bare "failed" — so the output is now shown, with the ephemeral home
// redacted, and the operation is still named.
func TestExitErrorSurfacesUnrecognisedOutput(t *testing.T) {
	e := &ExitError{Err: errors.New("exit status 1"), Stderr: "panic: boom"}
	if !strings.Contains(e.Error(), "annotation failed") {
		t.Errorf("Error() = %q, want it to name the operation", e.Error())
	}
	if !strings.Contains(e.Error(), "panic: boom") {
		t.Errorf("Error() = %q, want it to carry the process output", e.Error())
	}

	// The operation is named either way: "annotation failed" on a provisioning
	// job sends the reader looking in the wrong place.
	d := &ExitError{Err: errors.New("exit status 1"), Stderr: "panic", Op: "download"}
	if !strings.HasPrefix(d.Error(), "download failed") {
		t.Errorf("Error() = %q, want it to start with %q", d.Error(), "download failed")
	}

	if !strings.Contains(e.Detail(), "panic: boom") {
		t.Errorf("Detail() should carry the full diagnostic, got %q", e.Detail())
	}
}

func TestCLIMessageTruncates(t *testing.T) {
	// The cap is generous because the useful part of a multi-line error is
	// often the list rather than the first line — but it is still a cap.
	long := "error: " + strings.Repeat("x", 2000)
	got := cliMessage(long, "")
	if len(got) > 950 {
		t.Errorf("message not truncated: %d chars", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated message should be marked, got %q", got[len(got)-10:])
	}
}

func TestInventory(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, size int) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// varhub's cache layout: <cache>/<name>/<version>/...
	write("clinvar/2026-06/clinvar.vcf.gz", 100)
	write("clinvar/2026-06/clinvar.vcf.gz.tbi", 20)
	write("gencode/48/gencode.gtf.gz", 300)

	files, err := inventory(root)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3: %+v", len(files), files)
	}
	byPath := map[string]int64{}
	for _, f := range files {
		byPath[f.Path] = f.SizeBytes
		if f.ModifiedAt == 0 {
			t.Errorf("%s has no mtime", f.Path)
		}
		if filepath.IsAbs(f.Path) {
			t.Errorf("path %q should be relative to the storage root", f.Path)
		}
	}
	if byPath[filepath.Join("clinvar", "2026-06", "clinvar.vcf.gz")] != 100 {
		t.Errorf("sizes wrong: %+v", byPath)
	}

	// A location nothing has been downloaded into yet is empty, not an error —
	// the storage volume exists before the first download.
	missing, err := inventory(filepath.Join(root, "nope"))
	if err != nil || len(missing) != 0 {
		t.Errorf("missing root = %v, %v; want empty and no error", missing, err)
	}
}

func TestRewriteCacheDir(t *testing.T) {
	home := t.TempDir()
	cfg := `data_dir         = "/old/data"
cache_dir        = "/old/cache"
annotations_dir  = "./annotations"
default_snapshot = "dev"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rewriteCacheDir(home, "/mnt/sources", "/mnt/data"); err != nil {
		t.Fatalf("rewriteCacheDir: %v", err)
	}
	out, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	got := string(out)
	if !strings.Contains(got, `cache_dir        = "/mnt/sources"`) {
		t.Errorf("cache_dir not repointed:\n%s", got)
	}
	if !strings.Contains(got, `data_dir         = "/mnt/data"`) {
		t.Errorf("data_dir not repointed:\n%s", got)
	}
	// Untouched lines survive: the snapshot must still resolve.
	if !strings.Contains(got, `default_snapshot = "dev"`) ||
		!strings.Contains(got, `annotations_dir  = "./annotations"`) {
		t.Errorf("rewrite damaged the config:\n%s", got)
	}
}

func TestCleanup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gencode", "48")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.gz", "a.gz.tbi"} {
		if err := os.WriteFile(filepath.Join(dir, f), make([]byte, 500), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A sibling that must survive.
	other := filepath.Join(root, "clinvar", "2026-06")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "c.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	freed, err := Cleanup(CleanupRequest{Root: root, Name: "gencode", Version: "48"})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if freed != 1000 {
		t.Errorf("freed = %d, want 1000", freed)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("target directory survived")
	}
	// The empty parent goes too, but a sibling source is untouched.
	if _, err := os.Stat(filepath.Join(root, "gencode")); !os.IsNotExist(err) {
		t.Error("empty parent left behind")
	}
	if _, err := os.Stat(filepath.Join(other, "c.gz")); err != nil {
		t.Errorf("cleanup removed an unrelated source: %v", err)
	}

	// Removing something already gone is not an error.
	if _, err := Cleanup(CleanupRequest{Root: root, Name: "gencode", Version: "48"}); err != nil {
		t.Errorf("second cleanup: %v", err)
	}
}

// name/version come from catalog rows. A traversal in either must not be able to
// walk out of the storage root and delete something else.
func TestCleanupRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "..", "sentinel")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	for _, tc := range []CleanupRequest{
		{Root: root, Name: "..", Version: "sentinel"},
		{Root: root, Name: "../..", Version: "x"},
		{Root: root, Name: "a/b", Version: "1"},
		{Root: root, Name: "gencode", Version: "../.."},
		{Root: "", Name: "a", Version: "1"},
		{Root: root, Name: "a", Version: ""},
	} {
		if _, err := Cleanup(tc); err == nil {
			t.Errorf("Cleanup(%+v) should be refused", tc)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("cleanup escaped the storage root")
	}
}

// The report is how a download's result reaches the catalog. Walking the cache
// used to do this job and silently returned nothing for an object store, which
// is exactly the failure this parsing replaces.
func TestParseDownloadReport(t *testing.T) {
	out := []byte(`{
	  "cache": "s3://varhub-dev",
	  "snapshot": "provision",
	  "results": [
	    {"Source":"gencode:48","Data":"downloaded","Index":"built","files":[
	      {"path":"gencode/48/gencode.gtf.gz","size_bytes":79000000},
	      {"path":"gencode/48/gencode.gtf.gz.tbi","size_bytes":1650000}]},
	    {"Source":"builtins:1","Data":"-","Index":"-"}
	  ]}`)
	files, err := parseDownloadReport(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(files), files)
	}
	if files[0].Path != "gencode/48/gencode.gtf.gz" || files[0].SizeBytes != 79000000 {
		t.Errorf("first file = %+v", files[0])
	}
	// Paths must stay relative to the cache root, not become locators.
	for _, f := range files {
		if strings.Contains(f.Path, "://") {
			t.Errorf("path %q is a locator, not relative to the cache root", f.Path)
		}
	}
}

func TestParseDownloadReportRejectsGarbage(t *testing.T) {
	if _, err := parseDownloadReport([]byte("varhub: downloading...\n")); err == nil {
		t.Error("non-JSON output was accepted as a report")
	}
}

// Cleanup cannot remove objects, and must say so rather than reporting success
// after reclaiming nothing.
func TestCleanupRefusesObjectStore(t *testing.T) {
	_, err := Cleanup(CleanupRequest{Root: "s3://bucket/prefix", Name: "gencode", Version: "48"})
	if err == nil {
		t.Fatal("cleanup accepted an object-store root")
	}
	if !strings.Contains(err.Error(), "s3") {
		t.Errorf("error does not name the storage kind: %v", err)
	}
}

// varhub reports a problem on one line and its subjects on the next, indented.
// Keeping only the "error: " line left a message that named a failure and no
// cause — "sources not downloaded:" with nothing after the colon.
func TestCLIMessageKeepsContinuationLines(t *testing.T) {
	stderr := "varhub: loading snapshot\n" +
		"error: sources not downloaded — run `varhub download`:\n" +
		"  gencode:48 (missing /mnt/storage/gencode/48/gencode.gtf.gz)\n" +
		"  clinvar:1 (missing /mnt/storage/clinvar/1/clinvar.vcf.gz)\n"
	got := cliMessage(stderr, "")
	for _, want := range []string{"sources not downloaded", "gencode:48", "clinvar:1"} {
		if !strings.Contains(got, want) {
			t.Errorf("message lost %q:\n%s", want, got)
		}
	}
}

// Unrelated flush-left output after an error is not part of it.
func TestCLIMessageStopsAtUnindentedOutput(t *testing.T) {
	got := cliMessage("error: something failed\n  because of this\nvarhub: unrelated progress line\n", "")
	if !strings.Contains(got, "because of this") {
		t.Errorf("dropped the continuation: %q", got)
	}
	if strings.Contains(got, "unrelated progress") {
		t.Errorf("swallowed unrelated output: %q", got)
	}
}

// The ephemeral home is still redacted — it is a path the operator never chose
// and cannot act on.
func TestCLIMessageRedactsHome(t *testing.T) {
	got := cliMessage("error: open /tmp/varhub-home-123/x: no such file\n", "/tmp/varhub-home-123")
	if strings.Contains(got, "varhub-home-123") {
		t.Errorf("home not redacted: %q", got)
	}
}

// A failure with no recognisable "error:" line used to report "<op> failed" and
// nothing else — the case where a bare message is least useful, since there is
// no known shape to fall back on.
func TestMessageFallsBackToStderrTail(t *testing.T) {
	e := &ExitError{
		Op:   "download",
		Err:  errors.New("exit status 2"),
		Home: "/tmp/varhub-home-9182",
		Stderr: "fetching revel_all_chromosomes.csv.zip\n" +
			"unzipping into /tmp/varhub-home-9182/work\n" +
			"Traceback (most recent call last):\n" +
			"  File \"convert.py\", line 4, in <module>\n" +
			"MemoryError\n",
	}
	msg := e.Error()

	// The actual cause has to survive.
	if !strings.Contains(msg, "MemoryError") {
		t.Errorf("the failure's own output was dropped:\n%s", msg)
	}
	if !strings.Contains(msg, "download failed") {
		t.Errorf("message does not say what failed:\n%s", msg)
	}
	// The ephemeral home is meaningless to a reader and is redacted, as it is
	// in the recognised-error path.
	if strings.Contains(msg, "/tmp/varhub-home-9182") {
		t.Errorf("the temp home leaked into the message:\n%s", msg)
	}
	if !strings.Contains(msg, "<config>") {
		t.Errorf("redaction did not happen:\n%s", msg)
	}
}

// A recognised error still wins: the fallback must not bury a message that
// already names the problem in a wall of progress output.
func TestRecognisedErrorBeatsTheTail(t *testing.T) {
	e := &ExitError{
		Op:  "download",
		Err: errors.New("exit status 1"),
		Stderr: "varhub: fetching\nvarhub: unpacking\n" +
			"error: REVEL:1.3: required software not found on PATH: python3\n",
	}
	msg := e.Error()
	if msg != "REVEL:1.3: required software not found on PATH: python3" {
		t.Errorf("Error = %q", msg)
	}
}

// Nothing on stderr at all leaves the old behaviour, rather than an empty
// message that reads as a bug in the reporting.
func TestMessageWithNoStderr(t *testing.T) {
	e := &ExitError{Op: "download", Err: errors.New("signal: killed")}
	if got := e.Error(); got != "download failed" {
		t.Errorf("Error = %q, want %q", got, "download failed")
	}
}

// A successful run keeps its output too.
//
// Logs used to be stored only when the CLI exited non-zero, so the one case
// with nothing to read was a job that finished cleanly having annotated
// nothing — which is exactly what a snapshot pinning names no source emits
// looks like from the outside. The job is "done", the table is empty, and
// without this there is no way to tell "consulted the sources, no match" from
// "never consulted them".
func TestSuccessfulRunKeepsItsOutput(t *testing.T) {
	bin, home := testHome(t)
	r := &ExecRunner{Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second}

	res, err := r.Annotate(context.Background(), Request{
		Kind:       KindLocus,
		Snapshot:   "test",
		Selection:  "all",
		Body:       []byte("chr1:115256529:T:C"),
		OutputPath: outPath(t),
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if res.Log == "" {
		t.Fatal("a successful run recorded no output")
	}
	// It must be the run's own progress, not incidental noise: varhub prefixes
	// its progress lines, and that prefix is what makes the log worth keeping.
	if !strings.Contains(res.Log, "varhub:") {
		t.Errorf("log does not look like varhub progress output:\n%s", res.Log)
	}
	// The point of keeping it is learning what the run actually did.
	if !strings.Contains(res.Log, "annotat") {
		t.Errorf("log says nothing about annotating:\n%s", res.Log)
	}
}

// The archive destination must follow the job, not the catalog's default.
//
// The materializer fills tool_cache from whichever storage is default. A
// download is sent wherever the caller chose, and only an object store can hold
// an archive — so a default of "/mnt/storage" against a job going to
// "s3://bucket" means no archive at all, silently, because a filesystem path is
// not an archive destination. That is what produced nothing after a 24-hour VEP
// install with the setting showing as enabled.
func TestToolCacheFollowsTheJobsStorage(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("data_dir = \"/d\"\ncache_dir = \"/mnt/storage\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "annotations", "sources", "vep", "113")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "vep-113.locations.toml")
	if err := os.WriteFile(overlay,
		[]byte("# generated\ntool_cache = \"/mnt/storage\"\nannotation_prefix = \"VEP_\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rewriteCacheDir(home, "s3://bucket/prefix", "/data"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `tool_cache = "s3://bucket/prefix"`) {
		t.Errorf("tool_cache still points at the catalog default, so nothing will be "+
			"archived:\n%s", got)
	}
	// Everything else in the overlay survives.
	if !strings.Contains(string(got), `annotation_prefix = "VEP_"`) {
		t.Errorf("rewriting tool_cache dropped another setting:\n%s", got)
	}
}

// The cache is what makes a stale empty answer indistinguishable from a fresh
// one, so turning it off has to actually reach the CLI — asserted on the argv,
// because a flag that is silently dropped looks exactly like a cache that is
// working correctly.
func TestNoCacheReachesTheCLI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noCache bool
		want    bool
	}{
		{"absent by default", false, false},
		{"passed when set", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argv := filepath.Join(dir, "argv")
			// A stub varhub: records how it was called, then emits one variant so
			// the caller parses a result rather than failing on empty output.
			bin := filepath.Join(dir, "varhub")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argv + "\n" +
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

			// FixedHome requires a real config.toml; its contents do not matter
			// here because the CLI is a stub.
			hdir := t.TempDir()
			if err := os.WriteFile(filepath.Join(hdir, "config.toml"), []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r := &ExecRunner{
				Bin: bin, Home: FixedHome(hdir), Timeout: 30 * time.Second,
				NoCache: tc.noCache,
			}
			if _, err := r.Annotate(context.Background(), Request{
				Kind: KindLocus, Snapshot: "s", Selection: "all",
				Body:       []byte("chr1:1:A:T"),
				OutputPath: outPath(t),
			}); err != nil {
				t.Fatalf("Annotate: %v", err)
			}

			recorded, err := os.ReadFile(argv)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(string(recorded), "--no-cache")
			if got != tc.want {
				t.Errorf("--no-cache present = %v, want %v; argv was:\n%s",
					got, tc.want, recorded)
			}
		})
	}
}
