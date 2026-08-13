package jobstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeStore is the queue's side of a sweep: which jobs exist, and the lock.
type fakeStore struct {
	known  map[string]bool
	locked bool // true when someone else holds it
	took   bool
}

func (f *fakeStore) KnownChunkIDs(context.Context) (map[string]bool, error) {
	return f.known, nil
}

func (f *fakeStore) TryLock(context.Context, string) (bool, func(), error) {
	if f.locked {
		return false, func() {}, nil
	}
	f.took = true
	return true, func() {}, nil
}

// put writes an object under jobs/<id>/ with a given age.
func put(t *testing.T, root, id, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(root, "jobs", id, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("##fileformat=VCFv4.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The thing it is for: a file whose job is gone, old enough to be sure.
func TestAnOrphanedObjectIsRemoved(t *testing.T) {
	root := t.TempDir()
	orphan := put(t, root, "deadbeef", "input.vcf", 2*time.Hour)
	st := &fakeStore{known: map[string]bool{}}

	res, err := Sweep(context.Background(), st, root, DefaultGrace, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Errorf("removed %d, want 1 (%+v)", res.Removed, res)
	}
	if exists(orphan) {
		t.Error("the orphan is still there")
	}
}

// A file belonging to a job that still exists is never touched, however old.
//
// Age is not the test — ownership is. A queued job can sit for hours behind a
// long-running one, and collecting its input because it is old would fail a job
// that had not started yet.
func TestAnOwnedObjectSurvivesHoweverOld(t *testing.T) {
	root := t.TempDir()
	owned := put(t, root, "livejob", "input.vcf", 30*24*time.Hour)
	st := &fakeStore{known: map[string]bool{"livejob": true}}

	res, err := Sweep(context.Background(), st, root, DefaultGrace, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Errorf("removed %d; a month-old file of a live job is not an orphan", res.Removed)
	}
	if !exists(owned) {
		t.Fatal("the file of a job that still exists was deleted")
	}
}

// A recent orphan is left alone, because it is probably not an orphan.
//
// An upload is stored before its job row is written, so there is always a window
// where the file exists and the job does not. Collecting inside that window
// deletes the input of a submission in flight, which then fails with a missing
// file for a reason nobody can reconstruct.
func TestARecentOrphanIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	fresh := put(t, root, "justnow", "input.vcf", 5*time.Second)
	st := &fakeStore{known: map[string]bool{}}

	res, err := Sweep(context.Background(), st, root, DefaultGrace, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Errorf("removed %d; a five-second-old file may be an upload in progress", res.Removed)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped %d, want 1 — it should be counted, not silently ignored", res.Skipped)
	}
	if !exists(fresh) {
		t.Fatal("an upload in progress was deleted")
	}
}

// Dry run reports and removes nothing. The point of having one is to look at a
// storage bill before acting on it.
func TestADryRunRemovesNothing(t *testing.T) {
	root := t.TempDir()
	orphan := put(t, root, "deadbeef", "input.vcf", 2*time.Hour)
	st := &fakeStore{known: map[string]bool{}}

	res, err := Sweep(context.Background(), st, root, DefaultGrace, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Orphans != 1 {
		t.Errorf("orphans = %d, want 1; a dry run still has to report", res.Orphans)
	}
	if res.Removed != 0 || !exists(orphan) {
		t.Error("a dry run deleted something")
	}
}

// Anything not laid out as jobs/<id>/<name> is left alone. A collector must
// never delete what it does not understand.
func TestUnrecognizedLayoutIsNotTouched(t *testing.T) {
	root := t.TempDir()
	loose := filepath.Join(root, "jobs", "stray.txt")
	if err := os.MkdirAll(filepath.Dir(loose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(loose, old, old); err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{known: map[string]bool{}}

	res, err := Sweep(context.Background(), st, root, DefaultGrace, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 || !exists(loose) {
		t.Error("a file directly under jobs/ was collected; its shape says nothing about ownership")
	}
}

// When another replica holds the lock this does nothing at all — it does not
// list, and it certainly does not delete.
func TestASecondSweeperStandsAside(t *testing.T) {
	root := t.TempDir()
	orphan := put(t, root, "deadbeef", "input.vcf", 2*time.Hour)
	st := &fakeStore{known: map[string]bool{}, locked: true}

	res, err := Sweep(context.Background(), st, root, DefaultGrace, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 {
		t.Errorf("scanned %d; a replica that did not get the lock should not list the bucket",
			res.Scanned)
	}
	if !exists(orphan) {
		t.Error("two sweepers ran at once")
	}
}

// Storage that does not exist yet is an empty sweep, not a failure. A fresh
// deployment has taken no uploads.
func TestSweepingEmptyStorageIsNotAnError(t *testing.T) {
	st := &fakeStore{known: map[string]bool{}}
	res, err := Sweep(context.Background(), st, filepath.Join(t.TempDir(), "nothing"),
		DefaultGrace, false)
	if err != nil {
		t.Fatalf("sweeping a deployment with no uploads failed: %v", err)
	}
	if res.Scanned != 0 {
		t.Errorf("scanned %d objects in an empty store", res.Scanned)
	}
}
