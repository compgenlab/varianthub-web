package cacherunner

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/pgtest"
	"github.com/compgenlab/varianthub-web/internal/runner"
)

// --- a stand-in engine ---

// fakeEngine answers from a fixed table and records what it was asked.
//
// It honours the selection it is given and emits nothing else, which is what
// makes the tests about narrowing mean anything: a fake that always returned
// every field would pass whether or not the selection was narrowed at all.
type fakeEngine struct {
	mu     sync.Mutex
	calls  []runner.Request
	values map[string]map[string]any // locus key -> name -> value
	err    error
}

func (f *fakeEngine) Annotate(_ context.Context, req runner.Request) (runner.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.err != nil {
		return runner.Result{}, f.err
	}

	names := strings.Split(req.Selection, ",")
	var out []variant
	var args []string
	if req.Kind == runner.KindLocus {
		args = strings.Fields(string(req.Body))
	}
	for _, a := range args {
		parts := strings.Split(a, ":")
		pos, _ := strconv.ParseInt(parts[1], 10, 64)
		l := anncache.Locus{Chrom: parts[0], Pos: pos, Ref: parts[2], Alt: parts[3]}
		ann := map[string]any{}
		for _, n := range names {
			ann[n] = f.values[l.Key()][n]
		}
		out = append(out, variant{
			Chrom: l.Chrom, Pos: l.Pos, Ref: l.Ref, Alt: l.Alt, Annotations: ann,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return runner.Result{}, err
	}
	return runner.Result{Variants: b, N: len(out), Columns: describe(names), Log: "ran"}, nil
}

func (f *fakeEngine) Columns(_ context.Context, _ string, present map[string]bool) ([]runner.Column, error) {
	names := make([]string, 0, len(present))
	for n := range present {
		names = append(names, n)
	}
	sort.Strings(names)
	return describe(names), nil
}

func describe(names []string) []runner.Column {
	cols := make([]runner.Column, 0, len(names))
	for _, n := range names {
		cols = append(cols, runner.Column{Key: n, Label: n, Description: "described " + n})
	}
	return cols
}

func (f *fakeEngine) took() []runner.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runner.Request{}, f.calls...)
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

type harness struct {
	t      *testing.T
	engine *fakeEngine
	cache  *anncache.Store
	cat    *catalog.Store
	r      *Runner
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

// run annotates and returns the decoded result, keyed by locus.
func (h *harness) run(selection string, loci ...string) []variant {
	h.t.Helper()
	res, err := h.r.Annotate(context.Background(), runner.Request{
		Kind:      runner.KindLocus,
		Snapshot:  "snap",
		Selection: selection,
		Body:      []byte(strings.Join(loci, " ")),
	})
	if err != nil {
		h.t.Fatalf("Annotate(%q): %v", selection, err)
	}
	var out []variant
	if err := json.Unmarshal(res.Variants, &out); err != nil {
		h.t.Fatalf("bad result JSON: %v\n%s", err, res.Variants)
	}
	if res.N != len(loci) {
		h.t.Errorf("N = %d, want %d", res.N, len(loci))
	}
	return out
}

func vals(m map[string]map[string]any) map[string]map[string]any { return m }

// --- tests ---

// The whole point: the second identical job never starts the engine, so it never
// pays for a home, a source read or a container.
func TestASecondIdenticalJobNeverStartsTheEngine(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"auto_id": "chr1_100_A_T", "af": 0.25},
	}), "vbuiltins", "gnomad")

	first := h.run("auto_id,af", "chr1:100:A:T")
	if n := len(h.engine.took()); n != 1 {
		t.Fatalf("first run made %d engine call(s), want 1", n)
	}
	h.engine.reset()

	second := h.run("auto_id,af", "chr1:100:A:T")
	if n := len(h.engine.took()); n != 0 {
		t.Errorf("second run made %d engine call(s), want 0", n)
	}
	if first[0].Annotations["af"] != second[0].Annotations["af"] {
		t.Errorf("af changed between runs: %v then %v",
			first[0].Annotations["af"], second[0].Annotations["af"])
	}
	if second[0].Annotations["auto_id"] != "chr1_100_A_T" {
		t.Errorf("auto_id = %v, want chr1_100_A_T", second[0].Annotations["auto_id"])
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
	if got := strings.Fields(string(calls[0].Body)); len(got) != 1 || got[0] != "chr1:300:T:A" {
		t.Errorf("engine was given %v, want only the new variant", got)
	}
}

// dosage reads a sample's FORMAT column. Caching it would serve one sample's
// number as another's — wrong, plausible, and invisible in the result.
func TestASampleDependentBuiltinIsNeverCached(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{
		"chr1:100:A:T": {"auto_id": "chr1_100_A_T", "dosage": 1.0, "af": 0.25},
	}), "vbuiltins", "sbuiltins", "gnomad")

	h.run("auto_id,dosage,af", "chr1:100:A:T")
	h.engine.reset()

	got := h.run("auto_id,dosage,af", "chr1:100:A:T")
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
	for _, cached := range []string{"auto_id", "af"} {
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
		res, err := r.Annotate(ctx, runner.Request{
			Kind: runner.KindLocus, Snapshot: "snap", Selection: "auto_id,dosage,af",
			Body: []byte("chr1:100:A:T"),
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		var got []variant
		if err := json.Unmarshal(res.Variants, &got); err != nil {
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
		if _, err := r.Annotate(ctx, runner.Request{
			Kind: runner.KindLocus, Snapshot: "snap", Selection: "af",
			Body: []byte("chr1:100:A:T"),
		}); err != nil {
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
		// Every selected name is a key on every row, null where there is no
		// value — a caller cannot tell "no match" from "not selected" otherwise.
		for _, n := range []string{"af", "ac"} {
			if _, ok := v.Annotations[n]; !ok {
				t.Errorf("row %d is missing the key %q", i, n)
			}
		}
		if len(v.Annotations) != 2 {
			t.Errorf("row %d carries %v; only the selected names belong there", i, v.Annotations)
		}
	}
	if got[2].Annotations["af"] != nil {
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

	if _, err := h.r.Annotate(context.Background(), runner.Request{
		Kind: runner.KindLocus, Snapshot: "snap", Selection: "af", Body: []byte("chr1:100:A:T"),
	}); err != nil {
		t.Fatal(err)
	}
	h.engine.reset()

	res, err := h.r.Annotate(context.Background(), runner.Request{
		Kind: runner.KindLocus, Snapshot: "snap", Selection: "af", Body: []byte("chr1:100:A:T"),
	})
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

// The VCF path needs multi-allelic records split up front and recombined after,
// so until that lands it must run uncached rather than half-cached.
func TestVCFInputPassesThrough(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{}), "gnomad")
	if _, err := h.r.Annotate(context.Background(), runner.Request{
		Kind: runner.KindVCF, Snapshot: "snap", Selection: "af", Body: []byte("##fileformat=VCFv4.2\n"),
	}); err != nil {
		t.Fatal(err)
	}
	calls := h.engine.took()
	if len(calls) != 1 || calls[0].Kind != runner.KindVCF || string(calls[0].Body) != "##fileformat=VCFv4.2\n" {
		t.Errorf("the VCF request was not passed through verbatim: %+v", calls)
	}
}

func TestEngineFailureIsNotSwallowed(t *testing.T) {
	h := newHarness(t, vals(map[string]map[string]any{}), "gnomad")
	boom := errors.New("varhub exploded")
	h.engine.err = boom

	_, err := h.r.Annotate(context.Background(), runner.Request{
		Kind: runner.KindLocus, Snapshot: "snap", Selection: "af", Body: []byte("chr1:100:A:T"),
	})
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

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
