// Package cacherunner puts the shared annotation cache in front of the engine.
//
// It is a runner.Runner decorator: it answers what it can from stored values,
// asks varhub only for the rest, merges the two, and writes back what it learned.
// A job that hits completely never starts a process at all — no home to
// materialize, no sources to open, no container to start. That is the win a
// cache inside varhub cannot get, because by the time varhub is running, all of
// that has already been paid for.
//
// Nothing here can fail a job. Every cache error falls back to running the
// request unchanged, because a correct slow answer beats a fast wrong one and
// beats no answer at all.
package cacherunner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/runner"
)

// maxSelectionArg bounds the annotation list handed to the CLI as one argv
// entry, matching the runner's own limit. A selection expanded past it is not an
// error to report — it is a request to run uncached, which is always available.
const maxSelectionArg = 4096

// Runner wraps another runner with the cache.
type Runner struct {
	// Inner does the actual annotating. If it implements runner.ColumnLister,
	// a job answered entirely from cache can still describe its columns.
	Inner runner.Runner
	// Cache is the store. A nil Cache disables the decorator entirely.
	Cache *anncache.Store
	// Catalog attributes fields to sources. A nil Catalog disables it too.
	Catalog *catalog.Store
	// Site resolves the deployment's effective settings for this moment, so an
	// administrator turning the cache off takes effect on the next job rather
	// than on the next restart.
	Site func(ctx context.Context) catalog.Site
}

var _ runner.Runner = (*Runner)(nil)

// Annotate answers from the cache what it can and passes the rest to the engine.
func (r *Runner) Annotate(ctx context.Context, req runner.Request) (runner.Result, error) {
	p, ok := r.plan(ctx, req)
	if !ok {
		return r.Inner.Annotate(ctx, req)
	}

	hits, err := r.Cache.Lookup(ctx, p.assembly, p.loci, p.sourceRefs())
	if err != nil {
		log.Printf("cacherunner: lookup failed, running uncached: %v", err)
		return r.Inner.Annotate(ctx, req)
	}

	work := p.remaining(hits)
	if work.bail {
		return r.Inner.Annotate(ctx, req)
	}
	note(req, fmt.Sprintf("··· cache: %d/%d variant(s) served whole, %d source(s) skipped, asking varhub for %d",
		len(p.loci)-len(work.loci), len(p.loci), len(work.skipped), len(work.loci)))

	fresh, res, err := r.compute(ctx, req, p, work)
	if err != nil {
		return runner.Result{}, err
	}

	out, err := p.merge(hits, fresh)
	if err != nil {
		log.Printf("cacherunner: merge failed, running uncached: %v", err)
		return r.Inner.Annotate(ctx, req)
	}
	res.Variants, res.N = out, len(p.loci)
	res.Columns = r.columnsFor(ctx, req, p, res.Columns)

	// After the answer is assembled, so a cache that cannot be written still
	// returns the right result — the cost is only that the next job recomputes.
	//
	// The sweep follows the write and only the write: a job answered entirely
	// from cache added nothing to enforce a budget against, and the fastest path
	// in the system should not spend a round trip discovering that.
	if r.store(ctx, p, work, hits, fresh) {
		r.sweep(ctx)
	}
	return res, nil
}

// compute runs the engine over whatever the cache could not answer, and returns
// the fresh values by locus key alongside the Result to build on.
//
// A run with nothing left to ask is the point of all of this: no process, no
// home, no source reads. It still owes the caller a column model, which the
// inner runner can supply without annotating.
func (r *Runner) compute(ctx context.Context, req runner.Request, p *plan, work remainder) (map[string]map[string]any, runner.Result, error) {
	if len(work.loci) == 0 || len(work.ask) == 0 {
		note(req, "··· cache: answered entirely from cache; varhub not invoked")
		return nil, runner.Result{
			Log: "every variant answered from the shared annotation cache",
		}, nil
	}

	sub := req
	sub.Selection = strings.Join(work.ask, ",")
	sub.Body = []byte(strings.Join(work.args, " "))
	res, err := r.Inner.Annotate(ctx, sub)
	if err != nil {
		return nil, runner.Result{}, err
	}
	fresh, err := decodeVariants(res.Variants)
	if err != nil {
		return nil, runner.Result{}, err
	}
	return fresh, res, nil
}

// store writes back what the engine just computed, for the sources that may be
// cached and the loci that missed.
//
// Units are written even when they hold nothing. That empty unit is the record
// that a source was asked about a variant and had nothing to say, and without it
// the commonest variants — the ones no source annotates — are recomputed on
// every job forever.
//
// Reports whether anything was written, which is what makes a sweep worth doing.
func (r *Runner) store(ctx context.Context, p *plan, work remainder, hits anncache.Hits, fresh map[string]map[string]any) bool {
	if len(fresh) == 0 {
		return false
	}
	var units []anncache.Unit
	for _, l := range work.loci {
		key := l.Key()
		values, ok := fresh[key]
		if !ok {
			// The engine returned nothing for a locus it was given. Recording an
			// empty unit here would cache "nothing to say" for a run that did not
			// answer, so leave it uncached and let the next job ask again.
			continue
		}
		for _, sp := range p.cacheable {
			if work.skipped[sp.ref] {
				continue // not run; what is stored is still the whole answer
			}
			if _, cached := hits.Get(key, sp.ref); cached {
				continue // already stored, and the engine was not asked about it
			}
			entries := anncache.Hit{}
			for eff, manifest := range sp.fields {
				if v, ok := anncache.ValueOf(values[eff]); ok {
					entries[manifest] = v
				}
			}
			units = append(units, anncache.Unit{Locus: l, Source: sp.ref, Entries: entries})
		}
	}
	if len(units) == 0 {
		return false
	}
	if err := r.Cache.Put(ctx, p.assembly, units); err != nil {
		// The job's answer is already correct and already returned; the cost of a
		// failed write is that the next job recomputes.
		log.Printf("cacherunner: could not write %d unit(s): %v", len(units), err)
		return false
	}
	return true
}

// sweep enforces the deployment's cache budget, best effort.
func (r *Runner) sweep(ctx context.Context) {
	site := r.Site(ctx)
	if site.CacheMaxEntries == 0 && site.CacheMaxAge == 0 {
		return
	}
	res, err := r.Cache.Sweep(ctx, site.CacheMaxEntries, site.CacheMaxAge, 0)
	if err != nil {
		log.Printf("cacherunner: sweep: %v", err)
		return
	}
	if n := res.Removed(); n > 0 {
		log.Printf("cacherunner: evicted %d unit(s) (%d aged out, %d over the cap of %d)",
			n, res.ByAge, res.ByCount, site.CacheMaxEntries)
	}
}

// --- planning ---

// sourcePlan is one cacheable source and every field it contributes.
//
// Every field, not the selected ones. A unit is a source's whole answer for a
// variant, so it has to be written that way: cache only the fields one job asked
// for and the next job — asking for one more — finds the unit present, reads no
// value for the field it added, and reports a null the source would have
// answered. Over-fetching costs almost nothing next to it, because the expense
// of consulting a source is the seek, not the number of fields pulled out of
// what the seek found.
type sourcePlan struct {
	ref       string
	fields    map[string]string // effective name -> manifest name
	expensive bool
}

type plan struct {
	assembly  string
	loci      []anncache.Locus
	selected  []string // effective names the caller asked for, deduplicated, in order
	cacheable []*sourcePlan
	// passthrough are selected names no cacheable source produces. Their presence
	// means the engine must see every locus, whatever else is stored.
	passthrough []string
}

func (p *plan) sourceRefs() []string {
	out := make([]string, 0, len(p.cacheable))
	for _, sp := range p.cacheable {
		out = append(out, sp.ref)
	}
	return out
}

// plan works out what the cache can do for a request, or reports that it should
// stay out of the way.
//
// Bailing out is always correct and always available, so every uncertainty
// resolves that way: an input this cannot key, a snapshot it cannot read, a
// field it cannot attribute.
func (r *Runner) plan(ctx context.Context, req runner.Request) (*plan, bool) {
	if r.Cache == nil || r.Catalog == nil || r.Site == nil {
		return nil, false
	}
	if !r.Site(ctx).CacheEnabled {
		return nil, false
	}
	// The VCF path needs multi-allelic records split up front and the two halves
	// recombined afterwards; until that lands it runs uncached.
	if req.Kind != runner.KindLocus || req.Snapshot == "" {
		return nil, false
	}

	loci, ok := parseLoci(string(req.Body))
	if !ok {
		return nil, false
	}

	snap, fields, err := r.Catalog.SnapshotFields(ctx, req.Snapshot)
	if err != nil {
		log.Printf("cacherunner: cannot attribute %s, running uncached: %v", req.Snapshot, err)
		return nil, false
	}
	// Without an assembly a value cannot be keyed, and keying it anyway would
	// serve a GRCh37 answer to a GRCh38 question — silently, since the
	// coordinates match.
	if snap.Build == "" {
		return nil, false
	}

	selected := resolveSelection(req.Selection, snap, fields)
	if len(selected) == 0 {
		return nil, false
	}

	p := &plan{assembly: snap.Build, loci: loci, selected: selected}
	if !p.classify(fields) {
		return nil, false
	}
	if len(p.cacheable) == 0 {
		return nil, false // nothing to gain; do not pay for a lookup
	}
	return p, true
}

// classify sorts the selection into what may be cached and what may not.
//
// A source is cacheable only as a whole. Three things disqualify one:
//
//   - it produces a builtin that is not a function of the variant, so a stored
//     value would be one sample's number served to another;
//   - it shares an emitted name with another source, so a value cannot be
//     attributed to either without guessing;
//   - a selected name resolves to no known field, so this cannot account for
//     what the engine will emit.
//
// The last two set passthrough, which forces every locus to the engine — the
// answer is still correct, and the sources that are cacheable are still skipped.
func (p *plan) classify(fields []catalog.Field) bool {
	byName := map[string][]catalog.Field{}
	for _, f := range fields {
		byName[f.Name] = append(byName[f.Name], f)
	}
	// Every field of a source, so a unit is always its whole answer.
	allFields := map[string]map[string]string{}
	expensive := map[string]bool{}
	for _, f := range fields {
		if allFields[f.SourceRef] == nil {
			allFields[f.SourceRef] = map[string]string{}
		}
		allFields[f.SourceRef][f.Name] = f.Manifest
		expensive[f.SourceRef] = f.Expensive
	}

	banned := map[string]bool{}
	wanted := map[string]bool{}
	for _, name := range p.selected {
		found := byName[name]
		switch {
		case len(found) == 0:
			p.passthrough = append(p.passthrough, name)
			continue
		case len(found) > 1:
			// Ambiguous. Not cached rather than guessed, and neither claimant is
			// cacheable — a unit from either would carry a value the other might
			// have produced.
			p.passthrough = append(p.passthrough, name)
			for _, f := range found {
				banned[f.SourceRef] = true
			}
			continue
		}
		f := found[0]
		if f.Builtin != "" && !variantOnlyBuiltin(f.Builtin) {
			p.passthrough = append(p.passthrough, name)
			banned[f.SourceRef] = true
			continue
		}
		wanted[f.SourceRef] = true
	}

	refs := make([]string, 0, len(wanted))
	for ref := range wanted {
		if !banned[ref] {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs) // a stable plan, so two identical jobs ask identically
	for _, ref := range refs {
		p.cacheable = append(p.cacheable, &sourcePlan{
			ref: ref, fields: allFields[ref], expensive: expensive[ref],
		})
	}
	// A banned source's other fields still have to be asked for by name.
	//
	// One disqualifying field disqualifies the source, and a source's remaining
	// fields then have nowhere else to come from. This is the ordinary case, not
	// a corner: varhub's builtins arrive as a single source emitting auto_id
	// beside dosage. Without this the engine is never asked for auto_id and the
	// column comes back null on every row.
	for _, name := range p.selected {
		if found := byName[name]; len(found) == 1 && banned[found[0].SourceRef] {
			p.passthrough = append(p.passthrough, name)
		}
	}
	p.passthrough = dedup(p.passthrough)
	return true
}

// remainder is the work the cache could not do: which loci still need the
// engine, what to ask it for, and which sources it need not consult.
type remainder struct {
	loci    []anncache.Locus
	args    []string // the loci as the engine's own argv
	ask     []string // effective names to select
	skipped map[string]bool
	// bail asks the caller to run the original request untouched. Distinct from
	// an empty ask, which means the opposite — that there is nothing left to run.
	bail bool
}

// remaining works out the reduced request, in the order the wins matter.
//
// First drop variants nothing is missing for: those cost nothing at all. Then,
// across whatever is left, drop the sources that are cached for every one of
// them — which is the case of a source added to an existing snapshot, answered
// in a single invocation rather than one per source.
func (p *plan) remaining(hits anncache.Hits) remainder {
	work := remainder{skipped: map[string]bool{}}

	for _, l := range p.loci {
		if len(p.passthrough) == 0 && p.allCached(hits, l.Key()) {
			continue
		}
		work.loci = append(work.loci, l)
		work.args = append(work.args, l.Arg())
	}
	if len(work.loci) == 0 {
		return work
	}

	ask := append([]string{}, p.passthrough...)
	for _, sp := range p.cacheable {
		missing := false
		for _, l := range work.loci {
			if _, ok := hits.Get(l.Key(), sp.ref); !ok {
				missing = true
				break
			}
		}
		if !missing {
			work.skipped[sp.ref] = true
			continue
		}
		for name := range sp.fields {
			ask = append(ask, name)
		}
	}
	sort.Strings(ask)
	work.ask = dedup(ask)

	// One argv entry has a limit, and a selection expanded past it is not worth
	// failing over: hand the original request back instead, which is always
	// correct and merely uncached. Deliberately not an empty ask — that means
	// there is nothing left to run, which is the opposite instruction.
	if len(strings.Join(work.ask, ",")) > maxSelectionArg {
		return remainder{bail: true}
	}
	return work
}

func (p *plan) allCached(hits anncache.Hits, key string) bool {
	for _, sp := range p.cacheable {
		if _, ok := hits.Get(key, sp.ref); !ok {
			return false
		}
	}
	return true
}

// --- helpers ---

// variantOnlyBuiltin reports whether a builtin computes from chrom, pos, ref and
// alt alone, and may therefore be cached.
//
// varhub's annotate.VariantOnlyBuiltin is the authority. The rest read a
// sample's FORMAT columns — dosage, vaf, minor_strand, fisher_sb, copy_logratio
// — or the neighbouring variants in the stream, as vardist does. None of those
// is the same answer for two callers asking about the same locus, so a cached
// one hands one sample's number to another: wrong, entirely plausible, and
// invisible in the result.
//
// The list is copied rather than imported because varhub is a separate program
// this service execs, and taking a dependency on its packages for one function
// pulls in the filesystem- and container-bound closure the process boundary
// exists to keep out. What makes the copy safe is its direction: a builtin this
// has not heard of is not cached. A new sample-dependent builtin is then correct
// without anyone remembering this file, and a new variant-only one is merely
// uncached until someone does.
func variantOnlyBuiltin(name string) bool {
	switch name {
	case "auto_id", "indel", "tstv", "tags":
		return true
	}
	return false
}

// parseLoci reads the job's input, normalizing exactly as varhub does.
//
// Ref and alt are upper-cased because the engine upper-cases them, and the
// coordinates it echoes back are the normalized ones. Keying the cache off the
// raw text would file "chr1:100:a:t" and "chr1:100:A:T" separately while the
// engine treats them as one variant.
//
// Anything that is not chrom:pos:ref:alt means the cache cannot key this job,
// which is a reason to stand aside rather than to fail.
func parseLoci(body string) ([]anncache.Locus, bool) {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return nil, false
	}
	out := make([]anncache.Locus, 0, len(fields))
	for _, f := range fields {
		parts := strings.Split(f, ":")
		if len(parts) != 4 {
			return nil, false
		}
		pos, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, anncache.Locus{
			Chrom: parts[0], Pos: pos,
			Ref: strings.ToUpper(parts[2]), Alt: strings.ToUpper(parts[3]),
		})
	}
	return out, true
}

// resolveSelection turns a request's selection into the names it asks for.
//
// This mirrors how the engine reads the same string. The two disagreeing is not
// dangerous — a name resolved here and not there becomes a null the engine would
// have omitted, and a name missed here goes to passthrough — but it is worth
// keeping in step.
func resolveSelection(sel string, snap catalog.Snapshot, fields []catalog.Field) []string {
	switch strings.TrimSpace(sel) {
	case "":
		return dedup(snap.Defaults)
	case "all":
		names := make([]string, 0, len(fields))
		for _, f := range fields {
			names = append(names, f.Name)
		}
		return dedup(names)
	}
	var out []string
	for _, n := range strings.Split(sel, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return dedup(out)
}

// dedup keeps the first of each name, in order, in a slice of its own.
//
// A fresh slice rather than a filter in place: one caller passes a snapshot's
// Defaults straight in, and rewriting that would edit the catalog's own value
// through the copy it handed out.
func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func note(req runner.Request, line string) {
	if req.Sink != nil {
		req.Sink(line)
	}
}

func decodeVariants(b []byte) (map[string]map[string]any, error) {
	var rows []struct {
		Chrom       string         `json:"chrom"`
		Pos         int64          `json:"pos"`
		Ref         string         `json:"ref"`
		Alt         string         `json:"alt"`
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse engine output: %w", err)
	}
	out := make(map[string]map[string]any, len(rows))
	for _, r := range rows {
		l := anncache.Locus{Chrom: r.Chrom, Pos: r.Pos, Ref: r.Ref, Alt: r.Alt}
		out[l.Key()] = r.Annotations
	}
	return out, nil
}
