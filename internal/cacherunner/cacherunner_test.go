package cacherunner

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/pgtest"
	"github.com/compgenlab/varianthub-web/internal/runner"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// --- a stand-in engine ---

// fakeEngine answers from a fixed table and records what it was asked.
//
// It honours the selection it is given and emits nothing else, which is what
// makes the tests about narrowing mean anything: a fake that always returned
// every field would pass whether or not the selection was narrowed at all.
type fakeEngine struct {
	mu     sync.Mutex
	calls  []call
	values map[string]map[string]any // locus key -> name -> value
	err    error
}

// call is one request and the loci it actually carried.
//
// The loci are recorded here, at the moment of the call, because a reduced
// request travels as a file in a directory the decorator removes when it
// returns. A test that read InputPath afterwards would find nothing and report
// the narrowing as a failure to narrow.
type call struct {
	runner.Request
	loci []string
}

func (f *fakeEngine) Annotate(_ context.Context, req runner.Request) (runner.Result, error) {
	names := strings.Split(req.Selection, ",")
	// The staged file when there is one, exactly as the real runner does — it
	// prefers InputPath over a body, and a reduced request carries only the
	// file. A fake reading the body instead annotates nothing and every value
	// comes back null, which reads like a broken cache rather than a broken
	// stand-in.
	//
	// Through the same reader the decorator uses, which is how this stands in
	// for an engine that splits multi-allelic records the same way.
	var loci []anncache.Locus
	if req.InputPath != "" {
		loci, _ = parseVCFFile(req.InputPath)
	} else {
		loci, _ = parseInput(req)
	}

	keys := make([]string, 0, len(loci))
	for _, l := range loci {
		keys = append(keys, l.Key())
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{Request: req, loci: keys})
	f.mu.Unlock()

	if f.err != nil {
		return runner.Result{}, f.err
	}
	if req.OutputPath == "" {
		return runner.Result{}, errors.New("no output path")
	}

	// A VCF, because that is what the engine emits and what this decorator has
	// to read back. A fake returning values would let the tests pass against an
	// interface the real thing does not have.
	var b strings.Builder
	b.WriteString("##fileformat=VCFv4.2\n")
	for _, n := range names {
		if n == "" {
			continue
		}
		// Typed the way the real engine types them, because a value read back
		// out of a VCF is typed by its header: declare a score String and 0.25
		// comes back as the text "0.25", which compares unequal to the number a
		// caller put in and looks like a cache that lost precision.
		fmt.Fprintf(&b, "##INFO=<ID=%s,Number=A,Type=%s,Description=\"d\">\n", n, f.typeOf(n))
	}
	b.WriteString("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n")
	for _, l := range loci {
		var info []string
		for _, n := range names {
			v, ok := f.values[l.Key()][n]
			if !ok || v == nil {
				continue
			}
			info = append(info, fmt.Sprintf("%s=%v", n, v))
		}
		field := strings.Join(info, ";")
		if field == "" {
			field = "."
		}
		fmt.Fprintf(&b, "%s\t%d\t.\t%s\t%s\t.\t.\t%s\n", l.Chrom, l.Pos, l.Ref, l.Alt, field)
	}
	// Compressed, because the name says .gz and every reader here is told by
	// the name rather than sniffing. The real engine writes BGZF; plain gzip is
	// enough for a fake, since BGZF is gzip.
	out, err := os.Create(req.OutputPath)
	if err != nil {
		return runner.Result{}, err
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	if _, err := zw.Write([]byte(b.String())); err != nil {
		zw.Close()
		return runner.Result{}, err
	}
	if err := zw.Close(); err != nil {
		return runner.Result{}, err
	}
	return runner.Result{VCFPath: req.OutputPath, N: len(loci), Columns: f.describe(names), Log: "ran"}, nil
}

// typeOf declares a field the way the values it holds imply.
func (f *fakeEngine) typeOf(name string) string {
	for _, ann := range f.values {
		if v, ok := ann[name]; ok && v != nil {
			switch v.(type) {
			case float64, int, int64:
				return "Float"
			case bool:
				return "Flag"
			}
			return "String"
		}
	}
	return "String"
}

func (f *fakeEngine) Columns(_ context.Context, _ string, present map[string]bool) ([]runner.Column, error) {
	names := make([]string, 0, len(present))
	for n := range present {
		names = append(names, n)
	}
	sort.Strings(names)
	return f.describe(names), nil
}

// describe builds a column model like the one varhub reports, types included.
//
// The types matter and are easy to leave out: the answer's header is written
// from this model, so a column with no type is declared String and its value
// reads back as text. A test comparing 0.25 against the string "0.25" fails
// looking exactly like a cache that lost the number.
func (f *fakeEngine) describe(names []string) []runner.Column {
	cols := make([]runner.Column, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		typ := "text"
		if f.typeOf(n) == "Float" {
			typ = "numeric"
		}
		cols = append(cols, runner.Column{
			Key: n, Label: n, Type: typ, Description: "described " + n,
		})
	}
	return cols
}

func (f *fakeEngine) took() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call{}, f.calls...)
}

func (f *fakeEngine) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// --- fixture ---

const variantBuiltinsTOML = `[[sources]]
  type    = "builtin"
  name    = "vbuiltins"
  version = "1"

  [[sources.annotations]]
    builtin = "auto_id"
    name    = "auto_id"

  [[sources.annotations]]
    builtin = "tstv"
    name    = "tstv"
`

// dosage reads a sample's FORMAT column, so it is not a function of the variant.
const sampleBuiltinsTOML = `[[sources]]
  type    = "builtin"
  name    = "sbuiltins"
  version = "1"

  [[sources.annotations]]
    builtin = "dosage"
    name    = "dosage"
`

const gnomadTOML = `[[sources]]
  type    = "vcf"
  name    = "gnomad"
  version = "4.1"
  url     = "https://example.invalid/gnomad.vcf.gz"
  stream  = true

  [[sources.annotations]]
    name  = "af"
    field = "AF"
    type  = "numeric"

  [[sources.annotations]]
    name  = "ac"
    field = "AC"
    type  = "numeric"
`

const clinvarTOML = `[[sources]]
  type    = "vcf"
  name    = "clinvar"
  version = "2026-01"
  url     = "https://example.invalid/clinvar.vcf.gz"
  stream  = true

  [[sources.annotations]]
    name  = "clnsig"
    field = "CLNSIG"
`

// A tool: every query is a container start, which is what makes sparing it work
// worth an extra process.
const vepTOML = `[[sources]]
  type    = "tool"
  name    = "vep"
  version = "113"

  [[sources.annotations]]
    name  = "consequence"
    field = "Consequence"
`

type harness struct {
	t      *testing.T
	engine *fakeEngine
	cache  *anncache.Store
	cat    *catalog.Store
	r      *Runner
	answer string // the last file run wrote, for tests that read its header
}

// newHarness builds a catalog, a cache and a decorator over the fake engine.
func newHarness(t *testing.T, values map[string]map[string]any, sources ...string) *harness {
	t.Helper()
	pool := pgtest.Pool(t)
	cat, cache := catalog.New(pool), anncache.New(pool)
	ctx := context.Background()

	all := map[string]catalog.Source{
		"vbuiltins": {ID: "vbuiltins", Name: "vbuiltins", Version: "1", Kind: "builtin",
			Visibility: catalog.VisibilityPublic, TOML: variantBuiltinsTOML},
		"sbuiltins": {ID: "sbuiltins", Name: "sbuiltins", Version: "1", Kind: "builtin",
			Visibility: catalog.VisibilityPublic, TOML: sampleBuiltinsTOML},
		"gnomad": {ID: "gnomad", Name: "gnomad", Version: "4.1", Kind: "vcf", Build: "GRCh38",
			Visibility: catalog.VisibilityPublic, TOML: gnomadTOML},
		"clinvar": {ID: "clinvar", Name: "clinvar", Version: "2026-01", Kind: "vcf", Build: "GRCh38",
			Visibility: catalog.VisibilityPublic, TOML: clinvarTOML},
		"vep": {ID: "vep", Name: "vep", Version: "113", Kind: "tool", Build: "GRCh38",
			Visibility: catalog.VisibilityPublic, TOML: vepTOML},
	}
	if len(sources) == 0 {
		sources = []string{"vbuiltins", "sbuiltins", "gnomad", "clinvar"}
	}
	for _, id := range sources {
		src, ok := all[id]
		if !ok {
			t.Fatalf("no fixture source %q", id)
		}
		if err := cat.PutSource(ctx, src); err != nil {
			t.Fatalf("PutSource %s: %v", id, err)
		}
	}
	if err := cat.PutSnapshot(ctx, catalog.Snapshot{
		ID: "snap", Build: "GRCh38", State: catalog.StatePublished,
		Defaults: []string{"auto_id", "af"},
	}, sources); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	engine := &fakeEngine{values: values}
	return &harness{
		t: t, engine: engine, cache: cache, cat: cat,
		r: &Runner{
			Inner: engine, Cache: cache, Catalog: cat,
			Site: func(context.Context) catalog.Site {
				return catalog.Site{CacheEnabled: true}
			},
		},
	}
}

// variant is one annotated allele, read back out of the answer.
//
// A test convenience now rather than a wire format: the decorator's answer is a
// VCF, and this is what one record of it means once parsed.
type variant struct {
	Chrom       string
	Pos         int64
	Ref, Alt    string
	Annotations map[string]any
}

// run annotates and returns the answer, parsed back out of the VCF it wrote.
//
// The request carries both a body and a stored input file, which is what a job
// looks like now: the body is what the planning reads to key the cache, and the
// file is what the engine annotates and what the answer is assembled onto. They
// describe the same variants — a submission writes the file from the same list.
func (h *harness) run(selection string, loci ...string) []variant {
	h.t.Helper()
	dir := h.t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	writeLociVCF(h.t, in, loci)
	out := filepath.Join(dir, "out.vcf.gz")

	res, err := h.r.Annotate(context.Background(), runner.Request{
		Kind:       runner.KindLocus,
		Snapshot:   "snap",
		Selection:  selection,
		Body:       []byte(strings.Join(loci, " ")),
		InputPath:  in,
		OutputPath: out,
	})
	if err != nil {
		h.t.Fatalf("Annotate(%q): %v", selection, err)
	}
	if res.N != len(loci) {
		h.t.Errorf("N = %d, want %d", res.N, len(loci))
	}

	h.answer = res.VCFPath
	rows, err := vcfmerge.RowsFrom(res.VCFPath, 0)
	if err != nil {
		h.t.Fatalf("read the answer: %v", err)
	}
	got := make([]variant, 0, len(rows))
	for _, r := range rows {
		got = append(got, variant{
			Chrom: r.Chrom, Pos: r.Pos, Ref: r.Ref, Alt: r.Alt, Annotations: r.Annotations,
		})
	}
	return got
}

// writeLociVCF writes a locus list as the sites-only VCF a submission stores.
func writeLociVCF(t *testing.T, path string, loci []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("##fileformat=VCFv4.2\n#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n")
	for _, l := range loci {
		f := strings.Split(l, ":")
		if len(f) != 4 {
			t.Fatalf("not a locus: %q", l)
		}
		fmt.Fprintf(&b, "%s\t%s\t.\t%s\t%s\t.\t.\t.\n", f[0], f[1], f[2], f[3])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func vals(m map[string]map[string]any) map[string]map[string]any { return m }

// askedAbout returns the loci a request carried, read from the file it points at.
//
// The reduced request travels as a file, not a body: the decorator writes a
// filtered copy of the input and hands over its path. Asserting on Body would
// read the caller's whole submission and report every narrowing as a failure to
// narrow.
func askedAbout(t *testing.T, c call) []string {
	t.Helper()
	if c.InputPath == "" {
		t.Fatalf("the request carries no input file; body was %q", c.Body)
	}
	return c.loci
}

// declaredColumns reads the keys the answer's header declares.
//
// Where a caller used to look for a null on the row, it looks here: a VCF says
// what fields the file carries once, in the header, and omits them from any
// record that has no value. See TestOutputKeepsInputOrderAndSchema.
func (h *harness) declaredColumns() []string {
	h.t.Helper()
	src, err := openVCF(h.answer)
	if err != nil {
		h.t.Fatalf("open the answer: %v", err)
	}
	defer src.Close()
	rd, err := vcf.NewVcfReader(src)
	if err != nil {
		h.t.Fatalf("read the answer: %v", err)
	}
	hdr, err := rd.Header()
	if err != nil {
		h.t.Fatalf("read the answer's header: %v", err)
	}
	return vcfmerge.ColumnKeys(hdr)
}

// locusRequest is what a submitted locus list looks like by the time it reaches
// this decorator: a stored VCF, an output path, and the body the planner keys on.
func locusRequest(t *testing.T, selection string, loci ...string) runner.Request {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	writeLociVCF(t, in, loci)
	return runner.Request{
		Kind:       runner.KindLocus,
		Snapshot:   "snap",
		Selection:  selection,
		Body:       []byte(strings.Join(loci, " ")),
		InputPath:  in,
		OutputPath: filepath.Join(dir, "out.vcf.gz"),
	}
}

// --- tests ---

// The whole point: the second identical job never starts the engine, so it never
// pays for a home, a source read or a container.
func TestASecondIdenticalJobNeverStartsTheEngine(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"tstv": "ts", "af": 0.25},
	}), "vbuiltins", "gnomad")

	// tstv rather than auto_id, and the difference is the cache's rule rather
	// than the test's convenience: auto_id writes the record's ID column, so an
	// engine run leaves nothing in INFO for this to read back and store. See
	// cacheableBuiltin.
	first := h.run("tstv,af", "chr1:100:A:T")
	if n := len(h.engine.took()); n != 1 {
		t.Fatalf("first run made %d engine call(s), want 1", n)
	}
	h.engine.reset()

	second := h.run("tstv,af", "chr1:100:A:T")
	if n := len(h.engine.took()); n != 0 {
		t.Errorf("second run made %d engine call(s), want 0", n)
	}
	if first[0].Annotations["af"] != second[0].Annotations["af"] {
		t.Errorf("af changed between runs: %v then %v",
			first[0].Annotations["af"], second[0].Annotations["af"])
	}
	if second[0].Annotations["tstv"] != "ts" {
		t.Errorf("tstv = %v, want ts", second[0].Annotations["tstv"])
	}
}

// A unit is a source's whole answer, so it is written that way. Caching only the
// fields one job asked for would make the next job — asking for one more — read
// a null the source would have answered.
func TestASelectionCanGrowWithoutRecomputing(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25, "ac": 4.0},
	}), "gnomad")

	h.run("af", "chr1:100:A:T")
	h.engine.reset()

	got := h.run("af,ac", "chr1:100:A:T")
	if n := len(h.engine.took()); n != 0 {
		t.Errorf("adding a field from a cached source cost %d engine call(s), want 0", n)
	}
	if got[0].Annotations["ac"] != 4.0 {
		t.Errorf("ac = %v, want 4; a field of a cached source came back null",
			got[0].Annotations["ac"])
	}
}

// A source added to an existing snapshot: one invocation, asking only about the
// new source, with no extra process per source already cached.
func TestAddingASourceAsksOnlyAboutTheNewOne(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25, "ac": 4.0, "clnsig": "Pathogenic"},
		"chr1:200:G:C": {"af": 0.10, "ac": 2.0, "clnsig": "Benign"},
	}), "gnomad", "clinvar")

	h.run("af", "chr1:100:A:T", "chr1:200:G:C")
	h.engine.reset()

	got := h.run("af,clnsig", "chr1:100:A:T", "chr1:200:G:C")
	calls := h.engine.took()
	if len(calls) != 1 {
		t.Fatalf("made %d engine call(s), want 1", len(calls))
	}
	asked := strings.Split(calls[0].Selection, ",")
	if !contains(asked, "clnsig") {
		t.Errorf("selection %q does not ask for the new source's field", calls[0].Selection)
	}
	for _, cached := range []string{"af", "ac"} {
		if contains(asked, cached) {
			t.Errorf("selection %q still asks for %q, which was already cached",
				calls[0].Selection, cached)
		}
	}
	// And both loci still get both values.
	for _, v := range got {
		if v.Annotations["af"] == nil || v.Annotations["clnsig"] == nil {
			t.Errorf("%s:%d lost a value: %v", v.Chrom, v.Pos, v.Annotations)
		}
	}
}

func TestOnlyTheNewVariantsReachTheEngine(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25, "ac": 4.0},
		"chr1:200:G:C": {"af": 0.10, "ac": 2.0},
		"chr1:300:T:A": {"af": 0.05, "ac": 1.0},
	}), "gnomad")

	h.run("af", "chr1:100:A:T", "chr1:200:G:C")
	h.engine.reset()

	h.run("af", "chr1:100:A:T", "chr1:200:G:C", "chr1:300:T:A")
	calls := h.engine.took()
	if len(calls) != 1 {
		t.Fatalf("made %d engine call(s), want 1", len(calls))
	}
	if got := askedAbout(t, calls[0]); len(got) != 1 || got[0] != "chr1:300:T:A" {
		t.Errorf("engine was given %v, want only the new variant", got)
	}
}

// dosage reads a sample's FORMAT column. Caching it would serve one sample's
// number as another's — wrong, plausible, and invisible in the result.
func TestASampleDependentBuiltinIsNeverCached(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"tstv": "ts", "dosage": 1.0, "af": 0.25},
	}), "vbuiltins", "sbuiltins", "gnomad")

	h.run("tstv,dosage,af", "chr1:100:A:T")
	h.engine.reset()

	got := h.run("tstv,dosage,af", "chr1:100:A:T")
	calls := h.engine.took()
	if len(calls) != 1 {
		t.Fatalf("made %d engine call(s), want 1 — dosage must always be recomputed", len(calls))
	}
	asked := strings.Split(calls[0].Selection, ",")
	if !contains(asked, "dosage") {
		t.Errorf("selection %q dropped dosage; it cannot be served from cache", calls[0].Selection)
	}
	// The variant-only fields around it are still cached, so one uncacheable
	// field does not make the whole job uncacheable.
	for _, cached := range []string{"tstv", "af"} {
		if contains(asked, cached) {
			t.Errorf("selection %q still asks for %q, which is cacheable and cached",
				calls[0].Selection, cached)
		}
	}
	if got[0].Annotations["dosage"] != 1.0 {
		t.Errorf("dosage = %v, want 1", got[0].Annotations["dosage"])
	}

	// And nothing was written under the sample-dependent source.
	hits, err := h.cache.Lookup(context.Background(), "GRCh38",
		[]anncache.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}},
		[]string{"sbuiltins:1", "vbuiltins:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits.Get("chr1:100:A:T", "sbuiltins:1"); ok {
		t.Error("a sample-dependent builtin was written to the cache")
	}
	if _, ok := hits.Get("chr1:100:A:T", "vbuiltins:1"); !ok {
		t.Error("the variant-only builtins were not cached")
	}
}

// The realistic shape of the case above: varhub's builtins arrive as one source
// emitting auto_id beside dosage. One disqualifying field disqualifies the
// source, and every other field of it then has nowhere to come from but the
// engine — so the engine has to be asked for them by name.
func TestAMixedBuiltinSourceStillAsksForItsCacheableFields(t *testing.T) {
	pool := pgtest.Pool(t)
	cat, cache := catalog.New(pool), anncache.New(pool)
	ctx := context.Background()

	const mixedTOML = `[[sources]]
  type    = "builtin"
  name    = "builtins"
  version = "1"

  [[sources.annotations]]
    builtin = "auto_id"
    name    = "auto_id"

  [[sources.annotations]]
    builtin = "dosage"
    name    = "dosage"
`
	if err := cat.PutSource(ctx, catalog.Source{
		ID: "builtins", Name: "builtins", Version: "1", Kind: "builtin",
		Visibility: catalog.VisibilityPublic, TOML: mixedTOML,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.PutSource(ctx, catalog.Source{
		ID: "gnomad", Name: "gnomad", Version: "4.1", Kind: "vcf", Build: "GRCh38",
		Visibility: catalog.VisibilityPublic, TOML: gnomadTOML,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.PutSnapshot(ctx, catalog.Snapshot{
		ID: "snap", Build: "GRCh38", State: catalog.StatePublished,
	}, []string{"builtins", "gnomad"}); err != nil {
		t.Fatal(err)
	}

	engine := &fakeEngine{values: map[string]map[string]any{
		"chr1:100:A:T": {"auto_id": "chr1_100_A_T", "dosage": 1.0, "af": 0.25},
	}}
	r := &Runner{Inner: engine, Cache: cache, Catalog: cat,
		Site: func(context.Context) catalog.Site { return catalog.Site{CacheEnabled: true} }}

	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		in := filepath.Join(dir, "in.vcf")
		writeLociVCF(t, in, []string{"chr1:100:A:T"})
		res, err := r.Annotate(ctx, runner.Request{
			Kind: runner.KindLocus, Snapshot: "snap", Selection: "auto_id,dosage,af",
			Body:       []byte("chr1:100:A:T"),
			InputPath:  in,
			OutputPath: filepath.Join(dir, "out.vcf.gz"),
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got, err := vcfmerge.RowsFrom(res.VCFPath, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Annotations["auto_id"] != "chr1_100_A_T" {
			t.Fatalf("run %d: auto_id = %v; a field of a disqualified source was never asked for",
				i, got[0].Annotations["auto_id"])
		}
	}
	// The second run should still have skipped gnomad, which is cacheable.
	calls := engine.took()
	if len(calls) != 2 {
		t.Fatalf("made %d engine call(s), want 2", len(calls))
	}
	second := strings.Split(calls[1].Selection, ",")
	if !contains(second, "auto_id") || !contains(second, "dosage") {
		t.Errorf("second selection %q must carry the disqualified source's fields", calls[1].Selection)
	}
	if contains(second, "af") {
		t.Errorf("second selection %q still asks for the cached source", calls[1].Selection)
	}
}

// Two sources emitting the same name cannot be told apart, so neither is cached
// rather than one of them being guessed at.
func TestAnAmbiguousNameIsNotCached(t *testing.T) {
	pool := pgtest.Pool(t)
	cat, cache := catalog.New(pool), anncache.New(pool)
	ctx := context.Background()

	// A second source that also emits "af".
	const rivalTOML = `[[sources]]
  type    = "vcf"
  name    = "rival"
  version = "1"
  url     = "https://example.invalid/rival.vcf.gz"
  stream  = true

  [[sources.annotations]]
    name  = "af"
    field = "AF"
`
	for _, src := range []catalog.Source{
		{ID: "gnomad", Name: "gnomad", Version: "4.1", Kind: "vcf", Build: "GRCh38",
			Visibility: catalog.VisibilityPublic, TOML: gnomadTOML},
		{ID: "rival", Name: "rival", Version: "1", Kind: "vcf", Build: "GRCh38",
			Visibility: catalog.VisibilityPublic, TOML: rivalTOML},
	} {
		if err := cat.PutSource(ctx, src); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.PutSnapshot(ctx, catalog.Snapshot{
		ID: "snap", Build: "GRCh38", State: catalog.StatePublished, Defaults: []string{"af"},
	}, []string{"gnomad", "rival"}); err != nil {
		t.Fatal(err)
	}

	engine := &fakeEngine{values: map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25},
	}}
	r := &Runner{Inner: engine, Cache: cache, Catalog: cat,
		Site: func(context.Context) catalog.Site { return catalog.Site{CacheEnabled: true} }}

	for i := 0; i < 2; i++ {
		if _, err := r.Annotate(ctx, locusRequest(t, "af", "chr1:100:A:T")); err != nil {
			t.Fatalf("Annotate: %v", err)
		}
	}
	if n := len(engine.took()); n != 2 {
		t.Errorf("made %d engine call(s), want 2 — an ambiguous name must not be cached", n)
	}
	hits, err := cache.Lookup(ctx, "GRCh38",
		[]anncache.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}},
		[]string{"gnomad:4.1", "rival:1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("an ambiguous field was cached under %v", hits)
	}
}

// The caller pairs results with the rows it submitted by position, so the order
// has to survive a partial hit — where some rows come from the engine and some
// were never sent to it.
func TestOutputKeepsInputOrderAndSchema(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25, "ac": 4.0},
		"chr1:300:T:A": {"af": 0.05, "ac": 1.0},
		// chr1:200:G:C is annotated by nothing.
	}), "gnomad")

	h.run("af", "chr1:100:A:T", "chr1:200:G:C")
	h.engine.reset()

	got := h.run("af,ac", "chr1:300:T:A", "chr1:100:A:T", "chr1:200:G:C")
	want := []string{"chr1:300:T:A", "chr1:100:A:T", "chr1:200:G:C"}
	for i, v := range got {
		key := v.Chrom + ":" + strconv.FormatInt(v.Pos, 10) + ":" + v.Ref + ":" + v.Alt
		if key != want[i] {
			t.Errorf("row %d = %s, want %s", i, key, want[i])
		}
		// Only the selected names, and nothing the job did not ask for.
		for n := range v.Annotations {
			if n != "af" && n != "ac" {
				t.Errorf("row %d carries %q, which was not selected: %v", i, n, v.Annotations)
			}
		}
	}
	// The schema is the header's, not each row's. Under the JSON result every
	// selected name appeared on every row and was null where there was no value,
	// so that a caller could tell "no match" from "not selected". A VCF says it
	// once — the field is declared in the header and omitted from the records
	// that have no value for it — so that is where a caller reads the schema.
	declared := h.declaredColumns()
	for _, n := range []string{"af", "ac"} {
		if !contains(declared, n) {
			t.Errorf("the answer's header declares %v, missing the selected %q", declared, n)
		}
	}
	if _, ok := got[2].Annotations["af"]; ok {
		t.Errorf("a locus no source annotates came back with af = %v", got[2].Annotations["af"])
	}
}

// The empty unit at work: a variant nothing annotates must not be recomputed on
// every job, which is the case that otherwise never stops costing.
func TestAVariantNothingAnnotatesIsStillCached(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{}), "gnomad")

	h.run("af", "chr1:200:G:C")
	h.engine.reset()

	got := h.run("af", "chr1:200:G:C")
	if n := len(h.engine.took()); n != 0 {
		t.Errorf("made %d engine call(s), want 0 — an empty answer was not cached", n)
	}
	if got[0].Annotations["af"] != nil {
		t.Errorf("af = %v, want null", got[0].Annotations["af"])
	}
}

// A job answered whole still owes its caller a header.
func TestAFullyCachedJobStillDescribesItsColumns(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25},
	}), "gnomad")

	if _, err := h.r.Annotate(context.Background(),
		locusRequest(t, "af", "chr1:100:A:T")); err != nil {
		t.Fatal(err)
	}
	h.engine.reset()

	res, err := h.r.Annotate(context.Background(), locusRequest(t, "af", "chr1:100:A:T"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Columns) != 1 || res.Columns[0].Key != "af" {
		t.Fatalf("Columns = %+v, want one column for af", res.Columns)
	}
	if res.Columns[0].Description == "" {
		t.Error("the column lost its metadata; the table would show a bare key")
	}
}

func TestCacheOffPassesTheRequestThroughUntouched(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25},
	}), "gnomad")
	h.r.Site = func(context.Context) catalog.Site { return catalog.Site{CacheEnabled: false} }

	h.run("af", "chr1:100:A:T")
	h.run("af", "chr1:100:A:T")
	calls := h.engine.took()
	if len(calls) != 2 {
		t.Fatalf("made %d engine call(s), want 2", len(calls))
	}
	for i, c := range calls {
		if c.Selection != "af" || string(c.Body) != "chr1:100:A:T" {
			t.Errorf("call %d was rewritten: selection=%q body=%q", i, c.Selection, c.Body)
		}
	}
}

const twoAllelic = "##fileformat=VCFv4.2\n" +
	"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tS1\n" +
	"chr1\t100\t.\tA\tT,G\t.\tPASS\tDP=30\tGT\t0/1\n" +
	"chr1\t200\t.\tG\tC\t.\tPASS\tDP=12\tGT\t1/1\n"

func (h *harness) runVCF(selection, body string) []variant {
	h.t.Helper()
	dir := h.t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
	res, err := h.r.Annotate(context.Background(), runner.Request{
		Kind: runner.KindVCF, Snapshot: "snap", Selection: selection,
		Body:       []byte(body),
		InputPath:  in,
		OutputPath: filepath.Join(dir, "out.vcf.gz"),
	})
	if err != nil {
		h.t.Fatalf("Annotate: %v", err)
	}
	h.answer = res.VCFPath
	rows, err := vcfmerge.RowsFrom(res.VCFPath, 0)
	if err != nil {
		h.t.Fatalf("read the answer: %v", err)
	}
	out := make([]variant, 0, len(rows))
	for _, r := range rows {
		out = append(out, variant{
			Chrom: r.Chrom, Pos: r.Pos, Ref: r.Ref, Alt: r.Alt, Annotations: r.Annotations,
		})
	}
	return out
}

// A multi-allelic record is one variant per allele, which is how the engine reads
// it and therefore how the cache has to key it.
func TestVCFInputIsCachedPerAllele(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25, "ac": 4.0},
		"chr1:100:A:G": {"af": 0.05, "ac": 1.0},
		"chr1:200:G:C": {"af": 0.10, "ac": 2.0},
	}), "gnomad")

	first := h.runVCF("af", twoAllelic)
	if len(first) != 3 {
		t.Fatalf("got %d rows from a 2-record, 3-allele VCF, want 3", len(first))
	}
	if first[0].Alt != "T" || first[1].Alt != "G" || first[2].Alt != "C" {
		t.Errorf("allele order not preserved: %s %s %s", first[0].Alt, first[1].Alt, first[2].Alt)
	}
	if first[1].Annotations["af"] != 0.05 {
		t.Errorf("the second allele got %v, want 0.05 — alleles were not kept apart",
			first[1].Annotations["af"])
	}
	h.engine.reset()

	second := h.runVCF("af", twoAllelic)
	if n := len(h.engine.took()); n != 0 {
		t.Errorf("the same VCF twice made %d engine call(s), want 0", n)
	}
	for i := range second {
		if second[i].Annotations["af"] != first[i].Annotations["af"] {
			t.Errorf("row %d changed between runs: %v then %v",
				i, first[i].Annotations["af"], second[i].Annotations["af"])
		}
	}
}

// The reduced request stays a VCF: that path exists because a file holds more
// variants than argv can, and answering half of them must not undo that.
func TestAReducedVCFRequestIsStillAVCF(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"af": 0.25, "ac": 4.0},
		"chr1:100:A:G": {"af": 0.05, "ac": 1.0},
		"chr1:200:G:C": {"af": 0.10, "ac": 2.0},
	}), "gnomad")

	// Cache the first record's two alleles only.
	h.runVCF("af", "##fileformat=VCFv4.2\nchr1\t100\t.\tA\tT,G\t.\t.\t.\n")
	h.engine.reset()

	got := h.runVCF("af", twoAllelic)
	calls := h.engine.took()
	if len(calls) != 1 {
		t.Fatalf("made %d engine call(s), want 1", len(calls))
	}
	if calls[0].Kind != runner.KindVCF {
		t.Errorf("Kind = %q, want the request to stay a VCF", calls[0].Kind)
	}
	if sent := askedAbout(t, calls[0]); len(sent) != 1 || sent[0] != "chr1:200:G:C" {
		t.Errorf("engine was sent %v, want only the uncached allele", sent)
	}
	// And the answer still covers everything the caller submitted.
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	for i, v := range got {
		if v.Annotations["af"] == nil {
			t.Errorf("row %d lost its value", i)
		}
	}
}

func TestParseVCFMatchesTheEnginesReader(t *testing.T) {
	got, ok := parseVCF([]byte(twoAllelic))
	if !ok {
		t.Fatal("parseVCF refused a valid VCF")
	}
	want := []string{"chr1:100:A:T", "chr1:100:A:G", "chr1:200:G:C"}
	if len(got) != len(want) {
		t.Fatalf("got %d loci, want %d", len(got), len(want))
	}
	for i, l := range got {
		if l.Key() != want[i] {
			t.Errorf("locus %d = %q, want %q", i, l.Key(), want[i])
		}
	}
	// A "." alt is no allele, and a short line is something the engine rejects
	// outright — better to stand aside and let it say so than to key a guess.
	if got, _ := parseVCF([]byte("chr1\t100\t.\tA\t.\t.\t.\t.\n")); len(got) != 0 {
		t.Errorf("a missing ALT produced %v", got)
	}
	for _, bad := range []string{"", "##only=headers\n", "chr1\t100\t.\tA\n", "chr1\tx\t.\tA\tT\n"} {
		if _, ok := parseVCF([]byte(bad)); ok {
			t.Errorf("parseVCF(%q) accepted an input it cannot key", bad)
		}
	}
}

func TestEngineFailureIsNotSwallowed(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{}), "gnomad")
	boom := errors.New("varhub exploded")
	h.engine.err = boom

	_, err := h.r.Annotate(context.Background(), locusRequest(t, "af", "chr1:100:A:T"))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the engine's own error", err)
	}
}

func TestParseLociNormalizesTheWayTheEngineDoes(t *testing.T) {
	got, ok := parseLoci("chr1:100:a:t  chr2:5:G:C")
	if !ok {
		t.Fatal("parseLoci refused a valid input")
	}
	// The engine upper-cases ref and alt and echoes back the normalized form.
	// Keying off the raw text would file two spellings of one variant apart.
	if got[0].Key() != "chr1:100:A:T" {
		t.Errorf("key = %q, want chr1:100:A:T", got[0].Key())
	}
	for _, bad := range []string{"", "rs123", "chr1:100:A", "chr1:x:A:T", "chr1:100:A:T:extra"} {
		if _, ok := parseLoci(bad); ok {
			t.Errorf("parseLoci(%q) accepted an input it cannot key", bad)
		}
	}
}

// Asking a tool about variants it has already answered is the costliest thing
// this can get wrong: every one of them is a container start. So when it is owed
// for only some of the survivors, it gets an invocation of its own over exactly
// those.
func TestAnExpensiveSourceIsNotAskedAboutWhatItAlreadyKnows(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"tstv": "ts", "consequence": "missense", "af": 0.25, "ac": 4.0},
		"chr1:200:G:C": {"tstv": "ts", "consequence": "synonymous", "af": 0.10, "ac": 2.0},
		"chr1:300:T:A": {"tstv": "tv", "consequence": "stop_gained", "af": 0.05, "ac": 1.0},
	}), "vbuiltins", "gnomad", "vep")

	// The tool has answered for the first two variants and nothing else has.
	h.run("consequence", "chr1:100:A:T", "chr1:200:G:C")
	h.engine.reset()

	got := h.run("tstv,consequence,af", "chr1:100:A:T", "chr1:200:G:C", "chr1:300:T:A")
	calls := h.engine.took()
	if len(calls) != 2 {
		t.Fatalf("made %d engine call(s), want 2 — the tool was not given its own run", len(calls))
	}

	var toolRun, sharedRun *call
	for i := range calls {
		if contains(strings.Split(calls[i].Selection, ","), "consequence") {
			toolRun = &calls[i]
		} else {
			sharedRun = &calls[i]
		}
	}
	if toolRun == nil || sharedRun == nil {
		t.Fatalf("expected one tool run and one shared run, got %q and %q",
			calls[0].Selection, calls[1].Selection)
	}
	if given := askedAbout(t, *toolRun); len(given) != 1 || given[0] != "chr1:300:T:A" {
		t.Errorf("the tool was asked about %v, want only the variant it had not answered", given)
	}
	// The cheap sources are not split off; a second process costs more than
	// reading a few extra loci from local disk.
	if n := len(askedAbout(t, *sharedRun)); n != 3 {
		t.Errorf("the shared run covered %d variant(s), want 3", n)
	}
	// And the answer is whole.
	for i, v := range got {
		for _, name := range []string{"tstv", "consequence", "af"} {
			if v.Annotations[name] == nil {
				t.Errorf("row %d lost %s: %v", i, name, v.Annotations)
			}
		}
	}
}

// Splitting is only worth an extra process when there is a subset to spare. A
// source owed for every survivor has nothing to skip, so it rides along.
func TestASourceOwedForEverythingIsNotSplitOff(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"auto_id": "id1", "consequence": "missense"},
		"chr1:200:G:C": {"auto_id": "id2", "consequence": "synonymous"},
	}), "vbuiltins", "vep")

	h.run("auto_id,consequence", "chr1:100:A:T", "chr1:200:G:C")
	if n := len(h.engine.took()); n != 1 {
		t.Errorf("a cold job made %d engine call(s), want 1", n)
	}
}

// A capped split still answers everything; it just asks a wider question than it
// had to. Dropping the excess would silently lose values.
func TestTheRunCapFoldsBackRatherThanDropping(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"auto_id": "id1", "consequence": "missense", "af": 0.25, "ac": 4.0, "clnsig": "Pathogenic"},
		"chr1:200:G:C": {"auto_id": "id2", "consequence": "synonymous", "af": 0.10, "ac": 2.0, "clnsig": "Benign"},
		"chr1:300:T:A": {"auto_id": "id3", "consequence": "stop_gained", "af": 0.05, "ac": 1.0, "clnsig": "VUS"},
	}), "vbuiltins", "gnomad", "clinvar", "vep")
	h.r.MaxRuns = 1 // no budget for any split at all

	// Give each expensive source a different subset to be owed for.
	h.run("consequence", "chr1:100:A:T", "chr1:200:G:C")
	h.run("af", "chr1:100:A:T")
	h.engine.reset()

	got := h.run("auto_id,consequence,af,clnsig",
		"chr1:100:A:T", "chr1:200:G:C", "chr1:300:T:A")
	if n := len(h.engine.took()); n != 1 {
		t.Errorf("made %d engine call(s), want 1 under a cap of 1", n)
	}
	for i, v := range got {
		for _, name := range []string{"auto_id", "consequence", "af", "clnsig"} {
			if v.Annotations[name] == nil {
				t.Errorf("row %d lost %s under the cap: %v", i, name, v.Annotations)
			}
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
