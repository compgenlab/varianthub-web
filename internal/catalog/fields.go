package catalog

import (
	"context"
	"path/filepath"
	"strings"
)

// Field is one annotation a snapshot can emit, traced back to what produces it
// and how much producing it costs.
//
// Attribution is the part the annotation cache cannot do without. A cached value
// belongs to a source, not to a snapshot: two snapshots pinning the same source
// must share what it said, and a source dropped from a snapshot must take its
// values with it. The output name alone cannot express either — it is a property
// of the bundle, and it changes when a deployment sets an annotation_prefix.
type Field struct {
	// Name is the field as results will carry it, after any annotation_prefix.
	// This is what a selection names and what appears in the output JSON.
	Name string
	// Manifest is the source manifest's own name for the field, which is what a
	// value is cached under. Stable across a prefix change, so renaming output
	// does not invalidate anything already computed.
	Manifest string
	// SourceRef is "name:version" of the source that produces the field, and the
	// cache's identity for it.
	SourceRef string
	// SourceID is the catalog row, for callers that need the source itself.
	SourceID string
	// Builtin is the builtin annotator behind the field, empty for a field read
	// from data. Which builtins may be cached is a question about the annotation
	// engine rather than about the catalog, so it is left to the caller.
	Builtin string
	// Expensive reports that consulting this source costs a network round trip or
	// a container start, rather than a read from local disk.
	Expensive bool
}

// SnapshotFields returns a snapshot and every field its pinned sources can emit.
//
// One call rather than two because the caller invariably needs both: the
// snapshot carries the assembly, without which a cached value cannot be keyed at
// all, and pairing them here means they cannot be read from different moments.
func (s *Store) SnapshotFields(ctx context.Context, snapshot string) (Snapshot, []Field, error) {
	snap, err := s.GetSnapshot(ctx, snapshot)
	if err != nil {
		return Snapshot{}, nil, err
	}

	ids := make([]string, 0, len(snap.Sources))
	for _, src := range snap.Sources {
		ids = append(ids, src.ID)
	}
	// Where each source's files actually landed, which is what says whether
	// reading one touches the network.
	roots, err := s.StorageRootsForSources(ctx, ids)
	if err != nil {
		return Snapshot{}, nil, err
	}

	var out []Field
	for _, src := range snap.Sources {
		// Paired positionally: both lists are built from the same manifest in the
		// same order, so index i is the same annotation under two names. The same
		// pairing effectiveNames uses for a prefix rename.
		raw, eff := AnnotationsFromTOML(src.TOML), src.Annotations()
		expensive := expensiveSource(src, roots[src.ID])
		for i := range raw {
			if i >= len(eff) {
				break
			}
			out = append(out, Field{
				Name:      eff[i].Name,
				Manifest:  raw[i].Name,
				SourceRef: src.Ref(),
				SourceID:  src.ID,
				Builtin:   raw[i].Builtin,
				Expensive: expensive,
			})
		}
	}
	return snap, out, nil
}

// expensiveSource reports whether consulting a source costs more than a read
// from local disk.
//
// Three ways to be expensive, and they are not the same thing:
//
//   - streamed, so every query is a range request to somebody else's server;
//   - a tool, so a query means starting a container and running a program;
//   - held in storage that is not a filesystem on this machine.
//
// A builtin computes from the variant and touches nothing, which is why it is
// checked before the storage root: it has no files, so it has no root, and
// "unknown root" must not make the cheapest thing in the system look costly.
func expensiveSource(src Source, root string) bool {
	if src.Kind == "builtin" {
		return false
	}
	if src.Stream || src.Kind == "tool" {
		return true
	}
	return !localRoot(root)
}

// localRoot reports whether a storage root is a directory on this machine.
//
// A positive test for a local path, never a list of remote schemes to exclude.
// The two agree on everything except a scheme nobody thought of — and there the
// excluding version calls it local, which means a job issuing one range request
// per locus against a third party's server because a cost model guessed wrong.
// Unrecognized has to fall on the expensive side; the worst that costs is a
// cache that groups more carefully than it needed to.
func localRoot(root string) bool {
	if root == "" {
		return false // not provisioned anywhere, so nothing to call local
	}
	if strings.Contains(root, "://") {
		return false
	}
	return filepath.IsAbs(root)
}
