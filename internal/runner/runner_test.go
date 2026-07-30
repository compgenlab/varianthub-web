package runner

import (
	"context"
	"encoding/json"
	"errors"
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

func TestExecRunnerLocus(t *testing.T) {
	bin, home := testHome(t)
	r := &ExecRunner{Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second}

	res, err := r.Annotate(context.Background(), Request{
		Kind:      KindLocus,
		Snapshot:  "test",
		Selection: "all",
		Body:      []byte("chr1:115256529:T:C"),
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if res.N != 1 {
		t.Errorf("N = %d, want 1", res.N)
	}

	// The stored blob must be exactly what the CLI emitted, and must match the
	// []Variant schema the front-end consumes.
	var got []struct {
		Chrom       string         `json:"chrom"`
		Pos         int64          `json:"pos"`
		Ref         string         `json:"ref"`
		Alt         string         `json:"alt"`
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.Unmarshal(res.Variants, &got); err != nil {
		t.Fatalf("result is not a []Variant array: %v\n%s", err, res.Variants)
	}
	if len(got) != 1 || got[0].Chrom != "chr1" || got[0].Pos != 115256529 {
		t.Fatalf("unexpected variant: %+v", got)
	}
	if got[0].Annotations["auto_id"] != "chr1_115256529_T_C" {
		t.Errorf("auto_id = %v", got[0].Annotations["auto_id"])
	}
	if got[0].Annotations["tstv"] != "TS" {
		t.Errorf("tstv = %v", got[0].Annotations["tstv"])
	}
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
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if res.N != 2 {
		t.Errorf("N = %d, want 2", res.N)
	}
}

func TestExecRunnerProgressAndFailure(t *testing.T) {
	bin, home := testHome(t)

	var lines []string
	r := &ExecRunner{
		Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second,
		OnProgress: func(l string) { lines = append(lines, l) },
	}

	// A malformed locus must fail, and the failure must not leak the CLI's stderr
	// into the caller-facing message.
	_, err := r.Annotate(context.Background(), Request{
		Kind: KindLocus, Snapshot: "test", Selection: "all", Body: []byte("not-a-locus"),
	})
	if err == nil {
		t.Fatal("expected an error for a malformed locus")
	}
	if got := err.Error(); strings.Contains(got, home) || strings.Contains(got, "varhub:") {
		t.Errorf("caller-facing error leaks internals: %q", got)
	}
	var ee *ExitError
	if !asExitError(err, &ee) {
		t.Fatalf("expected *ExitError, got %T", err)
	}
	if ee.Detail() == "" {
		t.Error("ExitError.Detail() should carry the diagnostic for logs")
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

// A locus beginning with "-" must reach the CLI as a positional argument, not be
// swallowed as a flag. Without the "--" terminator this fails with "flag provided
// but not defined", which is a confusing way to report a bad variant.
func TestLeadingDashLocusIsNotAFlag(t *testing.T) {
	bin, home := testHome(t)
	r := &ExecRunner{Bin: bin, Home: FixedHome(home), Timeout: 60 * time.Second}

	_, err := r.Annotate(context.Background(), Request{
		Kind: KindLocus, Snapshot: "test", Selection: "all", Body: []byte("-oops"),
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

func TestExitErrorFallsBackToOpaque(t *testing.T) {
	e := &ExitError{Err: errors.New("exit status 1"), Stderr: "panic: boom"}
	if e.Error() != "annotation failed" {
		t.Errorf("Error() = %q, want the opaque fallback", e.Error())
	}
	// The fallback names the operation: "annotation failed" on a provisioning job
	// sends the reader looking in the wrong place.
	d := &ExitError{Err: errors.New("exit status 1"), Stderr: "panic", Op: "download"}
	if d.Error() != "download failed" {
		t.Errorf("Error() = %q, want %q", d.Error(), "download failed")
	}
	if !strings.Contains(e.Detail(), "panic: boom") {
		t.Errorf("Detail() should carry the full diagnostic, got %q", e.Detail())
	}
}

func TestCLIMessageTruncates(t *testing.T) {
	long := "error: " + strings.Repeat("x", 900)
	got := cliMessage(long, "")
	if len(got) > 420 {
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
