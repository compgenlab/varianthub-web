package cacherunner

import (
	"context"
	"errors"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/runner"
)

// The worker holds one runner and asks it to do several things. This type sits
// in front of that runner, so anything the engine can do and this cannot is a
// capability that silently disappears the moment the cache is enabled.
//
// That is not hypothetical: wrapping the runner removed Download, the worker
// asserted on the concrete *ExecRunner, and every provisioning job on every
// installation with a database configured was refused with "download requires
// the exec runner". Nothing failed until somebody tried to add a source.
//
// So this asserts the set, and fails when the engine grows a capability the
// wrapper has not been taught to pass on.
func TestTheWrapperKeepsEveryCapabilityTheEngineHas(t *testing.T) {
	var wrapped any = &Runner{Inner: &runner.ExecRunner{}}

	for _, c := range []struct {
		name string
		has  func(any) bool
	}{
		{"Runner (annotate)", func(v any) bool { _, ok := v.(runner.Runner); return ok }},
		{"Downloader (provision sources)", func(v any) bool { _, ok := v.(runner.Downloader); return ok }},
		{"ColumnLister (describe results)", func(v any) bool { _, ok := v.(runner.ColumnLister); return ok }},
		{"GeneLister (validate gene lists)", func(v any) bool { _, ok := v.(runner.GeneLister); return ok }},
	} {
		if !c.has(wrapped) {
			t.Errorf("the cache wrapper does not satisfy %s — enabling the cache "+
				"would take that capability away from the worker", c.name)
		}
		// And the thing it wraps really does have it, or the assertion above is
		// checking nothing.
		if !c.has(any(&runner.ExecRunner{})) {
			t.Errorf("ExecRunner itself does not satisfy %s; this test is vacuous", c.name)
		}
	}
}

// A wrapper around something that genuinely cannot download says so, rather than
// panicking or reporting a nil result as success.
func TestDownloadThroughAWrapperThatCannot(t *testing.T) {
	r := &Runner{Inner: &fakeEngine{}} // annotates, nothing else
	_, err := r.Download(context.Background(), runner.DownloadRequest{})
	if err == nil {
		t.Fatal("a runner that cannot download reported success")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("unexpected error kind: %v", err)
	}
}
