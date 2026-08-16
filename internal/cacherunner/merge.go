package cacherunner

import (
	"context"
	"log"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/runner"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
)

// merge assembles the answer: cached values, then whatever the engine computed
// over the top, keyed by allele.
//
// Order is not this function's problem any more, and that is worth saying. The
// answer is written by setting these values onto the submitted file record by
// record, so the order is the file's — which is the caller's, whatever it was.
// Nothing here has to reproduce it.
//
// Every selected name is a key on every variant, null where there is no value.
// That is the engine's own contract, and a caller that has to distinguish "no
// match" from "field not selected" can only do it if the schema is stable.
//
// Fresh values win over cached ones. In practice they cannot disagree — a source
// the engine was asked about was, by construction, one the cache had nothing for
// — but if they ever did, the one just computed is the one to trust.
func (p *plan) merge(hits anncache.Hits, fresh map[string]map[string]any) vcfmerge.Annotations {
	out := make(vcfmerge.Annotations, len(p.loci))
	for _, l := range p.loci {
		key := l.Key()
		ann := make(map[string]any, len(p.selected))
		for _, name := range p.selected {
			ann[name] = nil
		}

		for _, sp := range p.cacheable {
			hit, ok := hits.Get(key, sp.ref)
			if !ok {
				continue
			}
			for eff, manifest := range sp.fields {
				if _, want := ann[eff]; !want {
					continue // over-fetched for the cache, not asked for here
				}
				if v, ok := hit[manifest]; ok {
					ann[eff] = v.Any()
				}
			}
		}

		for name, v := range fresh[key] {
			if _, want := ann[name]; want {
				ann[name] = v
			}
		}
		out[key] = ann
	}
	return out
}

// columnsFor describes the selection, filling in what the engine could not.
//
// The engine only ever saw the narrowed request, so it can describe the fields
// it was asked to compute and nothing about the ones served from the cache. The
// gap is filled by asking it to describe those separately — which reads
// manifests and opens no source, so it costs a fraction of the run it replaced,
// and only happens when a source was actually skipped.
//
// A field left undescribed still gets a bare column. An unlabelled value in the
// table is a much smaller problem than a value that is not in the table at all.
func (r *Runner) columnsFor(ctx context.Context, req runner.Request, p *plan, inner []runner.Column) []runner.Column {
	have := map[string]bool{}
	var cols []runner.Column
	want := make(map[string]bool, len(p.selected))
	for _, n := range p.selected {
		want[n] = true
	}
	for _, c := range inner {
		if want[c.Key] && !have[c.Key] {
			have[c.Key] = true
			cols = append(cols, c)
		}
	}

	missing := map[string]bool{}
	for _, n := range p.selected {
		if !have[n] {
			missing[n] = true
		}
	}
	if len(missing) == 0 {
		return cols
	}

	if lister, ok := r.Inner.(runner.ColumnLister); ok {
		described, err := lister.Columns(ctx, req.Snapshot, missing)
		if err != nil {
			log.Printf("cacherunner: column metadata unavailable (%v); falling back to keys", err)
		}
		for _, c := range described {
			if missing[c.Key] && !have[c.Key] {
				have[c.Key] = true
				cols = append(cols, c)
			}
		}
	}

	// In the caller's own order, so the table reads the way the selection did.
	for _, n := range p.selected {
		if !have[n] {
			have[n] = true
			cols = append(cols, runner.Column{Key: n, Label: n})
		}
	}
	return cols
}
