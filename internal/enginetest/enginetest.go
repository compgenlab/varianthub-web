// Package enginetest builds a working varhub installation in a temp directory,
// so a test can annotate against a real engine instead of a stub.
//
// Everything this service does to a result — caching it, splitting it, merging
// it, converting it — is downstream of what varhub actually emits, and a stub
// that returns what the test expects cannot catch the case where the engine's
// real output is shaped differently. The failure this exists to catch is an
// annotation landing on the wrong variant, which no amount of asserting against
// one's own fixtures will find.
//
// Nothing here reaches the network. The source is a tabix-indexed VCF written in
// process by cghts, pinned with localpath so varhub never tries to download it,
// and the snapshot is a manifest written beside it. The one external requirement
// is a varhub binary; see Binary.
package enginetest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"
)

// Binary locates a varhub to run, and skips the test when there is none.
//
// Checked by running it rather than by stat: a path that exists and does not
// execute — the wrong architecture, a broken build — should skip in the same way
// as a missing one, not fail the suite with something that reads like a bug in
// the code under test.
//
// VARHUB_BIN names one explicitly; otherwise PATH. Building it here instead was
// rejected: it makes every test depending on this pay a Go build, and it would
// silently test a varhub other than the one the deployment ships.
func Binary(t *testing.T) string {
	t.Helper()
	bin := strings.TrimSpace(os.Getenv("VARHUB_BIN"))
	if bin == "" {
		found, err := exec.LookPath("varhub")
		if err != nil {
			t.Skip("no varhub on PATH and VARHUB_BIN is unset; " +
				"build one with `GOWORK=off go build -o /tmp/varhub ./cmd/varhub` in the cganno repo")
		}
		bin = found
	}
	if out, err := exec.Command(bin, "version").CombinedOutput(); err != nil {
		t.Skipf("%s does not run (%v): %s", bin, err, out)
	}
	return bin
}

// Annotation is one field a fixture source contributes.
type Annotation struct {
	Name  string // the INFO id varhub emits, and this service's column key
	Field string // the INFO id read from the source; defaults to Name
	Type  string // categorical | numeric | text | flag
}

// Record is one annotated position in a fixture source.
type Record struct {
	Chrom string
	Pos    int64
	Ref    string
	Alt    string
	// Info maps a source INFO id to its value, as text.
	Info map[string]string
}

// Home is a materialized varhub installation.
type Home struct {
	Dir      string // VARHUB_HOME
	Snapshot string // the snapshot id to annotate against
	Bin      string
}

// Fixture describes the installation to build.
type Fixture struct {
	Snapshot    string
	Assembly    string
	Annotations []Annotation
	Records     []Record
}

// Build writes a complete varhub home and returns how to use it.
//
// The layout is the one catalog.Materialize produces, because that is what the
// worker hands varhub in production: config.toml at the root, the snapshot under
// annotations/snapshots/, the source fragment under
// annotations/sources/<name>/<version>/. Diverging from it would test a shape
// nothing else creates.
func Build(t *testing.T, f Fixture) Home {
	t.Helper()
	bin := Binary(t)

	if f.Snapshot == "" {
		f.Snapshot = "testsnap"
	}
	if f.Assembly == "" {
		f.Assembly = "GRCh38"
	}
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "annotations", "sources", "fixture", "1")
	mustMkdir(t, srcDir)
	mustMkdir(t, filepath.Join(dir, "annotations", "snapshots"))

	data := writeSource(t, srcDir, f)

	write(t, filepath.Join(dir, "config.toml"), fmt.Sprintf(`# fixture
data_dir         = %q
cache_dir        = %q
tool_dir         = %q
annotations_dir  = "./annotations"
default_snapshot = %q
`, dir, dir, dir, f.Snapshot))

	names := make([]string, 0, len(f.Annotations))
	for _, a := range f.Annotations {
		names = append(names, fmt.Sprintf("%q", a.Name))
	}
	write(t, filepath.Join(dir, "annotations", "snapshots", f.Snapshot+".toml"),
		fmt.Sprintf(`title       = "fixture"
assembly    = %q
sources     = ["fixture:1"]
default_annotations = [%s]
`, f.Assembly, strings.Join(names, ", ")))

	// Proof the home is usable before any test asserts on annotation. A
	// misplaced file here fails every test that uses this, and the message
	// "no annotations selected" says nothing about which file was wrong.
	out, err := exec.Command(bin, "-home", dir, "annotation", "list", "--format", "json",
		"--", f.Snapshot).CombinedOutput()
	if err != nil {
		t.Fatalf("the fixture home is not usable: %v\n%s\nsource data: %s", err, out, data)
	}
	return Home{Dir: dir, Snapshot: f.Snapshot, Bin: bin}
}

// writeSource writes the tabix-indexed VCF the fixture annotates from, and the
// manifest pinning varhub to it.
func writeSource(t *testing.T, dir string, f Fixture) string {
	t.Helper()
	path := filepath.Join(dir, "fixture.vcf.gz")

	// A real tabix index, written by the same library that reads it. An
	// unindexed source is not a smaller version of this test — varhub queries by
	// region, so it would simply find nothing, which looks exactly like an
	// annotation that did not match.
	// One line at a time: the writer parses each to build the index, and sorts
	// them itself on Close — so the fixture's records need not be given in
	// coordinate order.
	w := tabix.NewWriter(path, tabix.NewWriterOpts().VCF().AutoIndex())
	var written strings.Builder
	// Header and data go in through different calls: Write parses a line to
	// index it, so a "##" line reaches it as a record with one field.
	head := func(line string) {
		w.WriteHeader(line)
		written.WriteString(line + "\n")
	}
	put := func(line string) {
		t.Helper()
		if err := w.Write(line); err != nil {
			t.Fatalf("write %q to the fixture source: %v", line, err)
		}
		written.WriteString(line + "\n")
	}

	head("##fileformat=VCFv4.2")
	for _, a := range f.Annotations {
		field := a.Field
		if field == "" {
			field = a.Name
		}
		head(fmt.Sprintf("##INFO=<ID=%s,Number=1,Type=String,Description=\"f\">", field))
	}
	head("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	for _, r := range f.Records {
		info := make([]string, 0, len(r.Info))
		// Sorted, so the fixture file is byte-identical run to run and a failure
		// diff shows the change under test rather than map ordering.
		for _, k := range sortedKeys(r.Info) {
			// Percent-encoded, because this is a VCF and an unescaped ";" ends
			// the INFO field. Writing it raw made the fixture itself malformed:
			// "SIG=risk;factor" read back as SIG=risk plus a bare flag called
			// "factor", and both engine paths agreed on the truncated value — so
			// the comparison passed while testing nothing.
			info = append(info, k+"="+escapeInfo(r.Info[k]))
		}
		field := strings.Join(info, ";")
		if field == "" {
			field = "."
		}
		put(fmt.Sprintf("%s\t%d\t.\t%s\t%s\t.\t.\t%s", r.Chrom, r.Pos, r.Ref, r.Alt, field))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write the fixture source: %v", err)
	}

	var man strings.Builder
	man.WriteString(fmt.Sprintf(`[[sources]]
name      = "fixture"
version   = "1"
assembly  = %q
format    = "vcf"
localpath = %q
`, f.Assembly, path))
	for _, a := range f.Annotations {
		man.WriteString(fmt.Sprintf("\n  [[sources.annotations]]\n  name = %q\n", a.Name))
		if a.Field != "" {
			man.WriteString(fmt.Sprintf("  field = %q\n", a.Field))
		}
		typ := a.Type
		if typ == "" {
			typ = "categorical"
		}
		man.WriteString(fmt.Sprintf("  type = %q\n", typ))
	}
	write(t, filepath.Join(dir, "fixture-1.toml"), man.String())
	return written.String()
}

// WriteIllegalFixture writes a home whose manifest declares a name that cannot
// be a VCF INFO key, for asserting that varhub refuses it.
//
// Built by hand rather than through Build, which validates the home before
// returning and would fail here — the refusal is the thing under test.
func WriteIllegalFixture(t *testing.T, dir string) {
	t.Helper()
	srcDir := filepath.Join(dir, "annotations", "sources", "fixture", "1")
	mustMkdir(t, srcDir)
	mustMkdir(t, filepath.Join(dir, "annotations", "snapshots"))

	write(t, filepath.Join(dir, "config.toml"), fmt.Sprintf(`data_dir = %q
cache_dir = %q
annotations_dir = "./annotations"
default_snapshot = "testsnap"
`, dir, dir))
	write(t, filepath.Join(dir, "annotations", "snapshots", "testsnap.toml"),
		`title = "fixture"
assembly = "GRCh38"
sources = ["fixture:1"]
default_annotations = ["gnomAD-AF"]
`)
	write(t, filepath.Join(srcDir, "fixture-1.toml"), fmt.Sprintf(`[[sources]]
name      = "fixture"
version   = "1"
assembly  = "GRCh38"
format    = "vcf"
localpath = %q

  [[sources.annotations]]
  name = "gnomAD-AF"
  field = "AF"
  type = "numeric"
`, filepath.Join(srcDir, "fixture.vcf.gz")))
}

// escapeInfo percent-encodes the characters an INFO value cannot carry.
func escapeInfo(s string) string {
	return strings.NewReplacer(
		"%", "%25", ":", "%3A", ";", "%3B", "=", "%3D", ",", "%2C",
		" ", "%20", "\t", "%09", "\r", "%0D", "\n", "%0A").Replace(s)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
