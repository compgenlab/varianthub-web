package cacherunner

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/pgtest"
	"github.com/compgenlab/varianthub-web/internal/runner"
)

// The cache reads a job's input itself, so it holds a second copy of the
// engine's VCF reader — and the two agreeing is not something a fake can
// establish. A variant split differently than the engine splits it is a value
// filed under a key the engine never asks about, and a row of the answer built
// from the wrong one. Neither shows up as an error.
//
// So this runs the real varhub, twice through the cache and once around it, and
// requires all three to agree. Set VHW_TEST_VARHUB to enable.
func TestCachedVCFMatchesTheEngineExactly(t *testing.T) {
	bin := os.Getenv("VHW_TEST_VARHUB")
	if bin == "" {
		t.Skip("VHW_TEST_VARHUB not set; skipping the engine agreement test")
	}
	pool := pgtest.Pool(t)
	cat, cache := catalog.New(pool), anncache.New(pool)
	ctx := context.Background()
	if err := cat.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	m := &catalog.Materializer{
		Store: cat, DataDir: t.TempDir(), CacheDir: t.TempDir(), Root: t.TempDir(),
	}
	exec := &runner.ExecRunner{Bin: bin, Home: m, Timeout: 60 * time.Second}
	cached := &Runner{
		Inner: exec, Cache: cache, Catalog: cat,
		Site: func(context.Context) catalog.Site { return catalog.Site{CacheEnabled: true} },
	}

	// Multi-allelic, out of order, with FORMAT and INFO columns the engine drops
	// and a "." allele it skips — the parts most easily read differently.
	const body = "##fileformat=VCFv4.2\n" +
		"##INFO=<ID=DP,Number=1,Type=Integer,Description=\"depth\">\n" +
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tS1\n" +
		"chr1\t115256529\t.\tT\tC,A\t.\tPASS\tDP=30\tGT\t0/1\n" +
		"chr1\t115256530\trs1\tG\t.\t.\tPASS\tDP=9\tGT\t0/0\n" +
		"chr7\t140753336\t.\tA\tT\t.\tPASS\tDP=44\tGT\t1/1\n"

	req := runner.Request{
		Kind: runner.KindVCF, Snapshot: "dev", Selection: "", Body: []byte(body),
	}

	direct, err := exec.Annotate(ctx, req)
	if err != nil {
		t.Fatalf("uncached run: %v", err)
	}
	first, err := cached.Annotate(ctx, req)
	if err != nil {
		t.Fatalf("first cached run: %v", err)
	}
	second, err := cached.Annotate(ctx, req)
	if err != nil {
		t.Fatalf("second cached run: %v", err)
	}

	want := rows(t, direct.Variants)
	if len(want) == 0 {
		t.Fatal("the engine returned no variants; the fixture is wrong, not the cache")
	}
	for _, got := range []struct {
		name string
		res  runner.Result
	}{{"first cached run", first}, {"second cached run", second}} {
		have := rows(t, got.res.Variants)
		if len(have) != len(want) {
			t.Fatalf("%s produced %d rows, the engine produced %d — the two readers "+
				"disagree about how this VCF splits", got.name, len(have), len(want))
		}
		for i := range want {
			if !equalRow(have[i], want[i]) {
				t.Errorf("%s row %d = %+v, engine = %+v", got.name, i, have[i], want[i])
			}
		}
		if got.res.N != len(want) {
			t.Errorf("%s: N = %d, want %d", got.name, got.res.N, len(want))
		}
	}
}

func rows(t *testing.T, b []byte) []variant {
	t.Helper()
	var out []variant
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad result JSON: %v\n%s", err, b)
	}
	return out
}

func equalRow(a, b variant) bool {
	if a.Chrom != b.Chrom || a.Pos != b.Pos || a.Ref != b.Ref || a.Alt != b.Alt {
		return false
	}
	if len(a.Annotations) != len(b.Annotations) {
		return false
	}
	for k, av := range a.Annotations {
		bv, ok := b.Annotations[k]
		if !ok || av != bv {
			return false
		}
	}
	return true
}
