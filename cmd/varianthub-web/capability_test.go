package main

import (
	"context"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/cacherunner"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/runner"
)

// The worker holds one runner.Runner and asks it for four different things. Three
// of them are capabilities the interface does not mention, so nothing about the
// type the worker holds says whether they are there — the assertion happens at
// the call site, inside a job, on a deployment.
//
// That is not a hypothetical gap. Wrapping the runner in the annotation cache
// removed Download, runDownload asserted on the concrete *ExecRunner, and every
// provisioning job on every installation with a database was refused for four
// releases. Both halves had tests; the *assembled* thing did not.
//
// So this asserts what withCache returns, in both shapes it can return, against
// the set the worker actually demands. It is the test whose absence cost those
// four releases.
func TestTheAssembledRunnerHasEveryCapabilityTheWorkerAsks(t *testing.T) {
	demands := []struct {
		name string
		has  func(runner.Runner) bool
	}{
		{"Runner.Annotate (the adapt() default branch)",
			func(r runner.Runner) bool { return r != nil }},
		{"Downloader (runDownload)",
			func(r runner.Runner) bool { _, ok := r.(runner.Downloader); return ok }},
		{"ColumnLister (describing a cached result)",
			func(r runner.Runner) bool { _, ok := r.(runner.ColumnLister); return ok }},
		{"GeneLister (cacheGTFGenes)",
			func(r runner.Runner) bool { _, ok := r.(runner.GeneLister); return ok }},
	}

	// Both shapes withCache can return. The bare engine is what a deployment with
	// no database gets; the wrapper is what every deployment with one gets, which
	// is the shape that broke.
	for _, shape := range []struct {
		name string
		r    runner.Runner
	}{
		{"the bare engine (no database configured)", &runner.ExecRunner{}},
		{"the cache wrapper (a database is configured)",
			&cacherunner.Runner{Inner: &runner.ExecRunner{}}},
	} {
		for _, d := range demands {
			if !d.has(shape.r) {
				t.Errorf("%s does not satisfy %s — the worker would fail that job "+
					"at run time with no test having noticed", shape.name, d.name)
			}
		}
	}
}

// And the shape really is one of those two, rather than something this test has
// imagined. withCache with no database returns the engine it was handed; that is
// the branch reachable without a live Postgres, and it pins the contract that the
// return value is the engine itself rather than a wrapper around it.
func TestWithCacheReturnsTheEngineWhenThereIsNoDatabase(t *testing.T) {
	exec := &runner.ExecRunner{}
	got, cleanup, err := withCache(context.Background(), &config.Config{}, nil, exec)
	if err != nil {
		t.Fatalf("withCache: %v", err)
	}
	defer cleanup()
	if got != runner.Runner(exec) {
		t.Errorf("withCache returned %T, want the engine unchanged", got)
	}
}
