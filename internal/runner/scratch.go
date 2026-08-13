package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScratchMaxAge is how long a workdir may sit untouched before a starting
// worker treats it as abandoned.
//
// Generous, because the cost of being wrong in each direction is asymmetric: a
// live build whose directory is removed loses one chunk, which requeues; a
// stale one left behind is permanent. A build step that touches nothing under
// its own workdir for half a day is not a step that is still running.
const ScratchMaxAge = 12 * time.Hour

// SweepScratch removes abandoned varhub workdirs from dir, returning how many it
// removed and how many bytes that freed.
//
// varhub creates these with os.MkdirTemp and removes them with a deferred
// cleanup, which covers every path it can return from — and none where it is
// killed. A rolling restart does exactly that: SIGKILL to a worker mid-build
// orphans a directory holding a source's inputs and its converted output, which
// for a large source is gigabytes. Nothing ever reclaimed them, so a deployment
// that redeploys often accumulated one per interrupted build until the volume
// filled, at which point every pod on the node started being evicted for disk
// pressure and each eviction orphaned another.
//
// Age rather than ownership, because workers share this directory: a pod cannot
// tell its peers' live builds from its own dead ones by name. Anything still
// being written to has a recent mtime somewhere inside it.
func SweepScratch(dir string, maxAge time.Duration, logf func(string, ...any)) (int, int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	var n int
	var freed int64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "varhub-") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		newest, size, err := newestMTime(p)
		if err != nil {
			continue // vanished, or unreadable — not ours to force
		}
		if newest.After(cutoff) {
			continue // someone is still working in here
		}
		if err := os.RemoveAll(p); err != nil {
			logf("could not remove abandoned workdir %s: %v", p, err)
			continue
		}
		n++
		freed += size
	}
	return n, freed, nil
}

// newestMTime reports the most recent modification time anywhere under root, and
// the total size of its regular files.
//
// The whole tree, not just the directory itself: a directory's own mtime changes
// only when an entry is added or removed at that level, so a build spending an
// hour writing one output file leaves the top unchanged the entire time. Judging
// by that alone would delete exactly the long-running builds this is meant to
// leave alone.
func newestMTime(root string) (time.Time, int64, error) {
	var newest time.Time
	var size int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip what we cannot read rather than failing the sweep
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, 0, err
	}
	if newest.IsZero() {
		return time.Time{}, 0, fmt.Errorf("%s: no entries", root)
	}
	return newest, size, nil
}

// chunkScratchPrefix marks a scratch directory as belonging to one chunk.
//
// The string still says "job", from when a chunk was called one. It is a name
// on disk rather than a name in the code, and this sweep is the only thing that
// reclaims these directories — the age-based one above matches "varhub-" and
// would never see them. Changing it would strand every directory a running
// deployment had already written.
const chunkScratchPrefix = "vhw-job-"

// ChunkScratchDir is where a chunk's varhub invocation stages its work.
//
// Named after the chunk so that what it leaves behind can be attributed. varhub
// removes its own temp dirs on every path it can return from, and none where it
// is killed — an OOM or a rolling restart orphans whatever it had staged. Age
// alone cannot judge those: workers share this directory, so a recent orphan
// and a peer's live build look identical.
func ChunkScratchDir(chunkID string) string {
	return filepath.Join(os.TempDir(), chunkScratchPrefix+chunkID)
}

// SweepChunkScratch removes chunk-scoped scratch for chunks that are no longer
// running, returning how many directories it removed and the bytes freed.
//
// live is the set of chunk ids the queue still has running. Anything else is
// finished, failed or abandoned, and its scratch is reclaimable immediately —
// no age threshold, because the queue is authoritative about what is in flight
// and a directory carries the id to check against it.
func SweepChunkScratch(dir string, live map[string]bool, logf func(string, ...any)) (int, int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	var n int
	var freed int64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), chunkScratchPrefix) {
			continue
		}
		id := strings.TrimPrefix(e.Name(), chunkScratchPrefix)
		if live[id] {
			continue // a worker somewhere is still running this
		}
		p := filepath.Join(dir, e.Name())
		_, size, err := newestMTime(p)
		if err != nil {
			size = 0
		}
		if err := os.RemoveAll(p); err != nil {
			logf("could not remove scratch for chunk %s: %v", id, err)
			continue
		}
		n++
		freed += size
	}
	return n, freed, nil
}
