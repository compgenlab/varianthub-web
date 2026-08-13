// Package jobstore collects files in job storage that no job owns any more.
//
// The TTL sweep already removes the input and the result of an expiring job: it
// reads their locations out of the rows before deleting them. What it cannot
// reach is anything whose row was never written — an upload stored a moment
// before the process died, a chunk written by a split that failed partway. There
// is nothing in the database pointing at those, so nothing keyed on the database
// will ever find them.
//
// This finds them the only way left: by listing what is there and asking which
// job each file claims to belong to. That works because the layout says so —
// jobs/<job-id>/<name> — which is why it was chosen over naming objects after
// their contents.
package jobstore

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/compgenlab/varianthub-web/internal/blob"
)

// LockName is the advisory lock that keeps replicas from sweeping at once.
const LockName = "varhub-job-storage-sweep"

// DefaultGrace is how new an object has to be to be left alone.
//
// An upload is stored before the job row is written, so there is always a
// window where a file exists and its job does not — that is the normal path,
// not an orphan. An hour is far longer than that window and far shorter than
// the TTL, so nothing real is ever collected and nothing dead survives long.
//
// The failure this prevents is the worst one available here: deleting the input
// of a job somebody submitted two seconds ago, which then fails with a missing
// file for a reason nobody can reconstruct.
const DefaultGrace = time.Hour

// Store is what a sweep needs from the queue.
type Store interface {
	KnownChunkIDs(ctx context.Context) (map[string]bool, error)
	TryLock(ctx context.Context, name string) (bool, func(), error)
}

// Result is what one sweep did.
type Result struct {
	Scanned int   // objects under the prefix
	Orphans int   // those belonging to no job and older than the grace period
	Removed int   // of those, the ones actually deleted
	Bytes   int64 // freed
	Skipped int   // orphaned but too recent to touch
}

// Sweep removes files in job storage whose job no longer exists.
//
// Reports what it found even in dry-run, which is the point of having one: an
// operator chasing a storage bill wants to know what would go before it goes.
func Sweep(ctx context.Context, st Store, jobStorage string, grace time.Duration,
	dryRun bool) (Result, error) {

	var res Result

	ok, release, err := st.TryLock(ctx, LockName)
	if err != nil {
		return res, err
	}
	if !ok {
		// Another replica is doing it. Nothing to wait for: by the time it
		// finishes, the work is finished.
		return res, nil
	}
	defer release()

	// Read the job ids *before* listing storage, never after.
	//
	// A job created during the listing has its files seen and its row missed,
	// and would be collected. Reading first inverts the race into the harmless
	// direction: a job created after this snapshot is simply not in it, its
	// files are new, and the grace period covers them.
	known, err := st.KnownChunkIDs(ctx)
	if err != nil {
		return res, err
	}

	prefix := strings.TrimRight(jobStorage, "/") + "/jobs/"
	objects, err := blob.List(ctx, prefix)
	if err != nil {
		return res, err
	}
	res.Scanned = len(objects)

	cutoff := time.Now().Add(-grace)
	for _, o := range objects {
		id := jobIDOf(o.URI, prefix)
		if id == "" || known[id] {
			continue
		}
		res.Orphans++
		if o.ModTime.After(cutoff) {
			res.Skipped++
			continue
		}
		if dryRun {
			continue
		}
		if err := blob.Remove(ctx, o.URI); err != nil {
			log.Printf("jobstore: could not remove %s: %v", o.URI, err)
			continue
		}
		res.Removed++
		res.Bytes += o.Size
	}
	return res, nil
}

// jobIDOf extracts the job id from an object's location, or "" when the object
// is not under a job at all.
//
// Anything unrecognized is left alone rather than guessed at. A file directly
// under jobs/, or one nested deeper than the layout describes, was put there by
// something this does not know about — and the one thing a collector must never
// do is delete what it does not understand.
func jobIDOf(uri, prefix string) string {
	rest, ok := strings.CutPrefix(uri, prefix)
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		return ""
	}
	return id
}

// Start runs a sweep on a timer until ctx ends.
//
// In the worker rather than as a separate scheduled job, matching the queue's
// own sweeper: this needs the database and job storage, and the worker is the
// process that already has both configured. The advisory lock inside Sweep is
// what keeps several workers from each listing the whole bucket.
func Start(ctx context.Context, st Store, jobStorage string, every time.Duration) {
	if every <= 0 {
		return
	}
	go func() {
		// Not immediately on boot. A rolling restart brings several workers up
		// at once, and the first thing they would all do is contend for the
		// lock and list a bucket — while the queue they exist to drain waits.
		t := time.NewTimer(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			res, err := Sweep(ctx, st, jobStorage, DefaultGrace, false)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("jobstore: sweep: %v", err)
				}
			} else if res.Removed > 0 || res.Skipped > 0 {
				log.Printf("jobstore: swept %d object(s), removed %d (%.1f MB), "+
					"left %d too recent to be sure",
					res.Scanned, res.Removed, float64(res.Bytes)/(1<<20), res.Skipped)
			}
			t.Reset(every)
		}
	}()
}
