package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkTree(t *testing.T, dir string, age time.Duration, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "inputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "inputs", "big.gz")
	if err := os.WriteFile(f, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	for _, p := range []string{f, filepath.Join(dir, "inputs"), dir} {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

// The case that filled a 492 GB volume: workdirs from builds whose process was
// killed, which nothing ever reclaimed.
func TestSweepRemovesAbandonedWorkdirs(t *testing.T) {
	dir := t.TempDir()
	mkTree(t, filepath.Join(dir, "varhub-build-1"), 48*time.Hour, 1024)
	mkTree(t, filepath.Join(dir, "varhub-provision-2"), 48*time.Hour, 2048)

	n, freed, err := SweepScratch(dir, ScratchMaxAge, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	if freed < 3072 {
		t.Errorf("freed %d bytes, want at least 3072", freed)
	}
	for _, name := range []string{"varhub-build-1", "varhub-provision-2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived", name)
		}
	}
}

// A build in progress must survive. Workers share this directory, so a starting
// pod is looking at its peers' live work as well as its own dead work.
func TestSweepLeavesLiveWorkdirs(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "varhub-build-live")
	mkTree(t, live, 2*time.Minute, 1024)

	n, _, err := SweepScratch(dir, ScratchMaxAge, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("removed %d, want 0", n)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live workdir was removed: %v", err)
	}
}

// A long build writing one output file leaves the top-level mtime untouched for
// hours. Judging by the directory alone would delete exactly the long-running
// builds this must not touch.
func TestSweepLooksInsideNotJustAtTheTop(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "varhub-build-slow")
	mkTree(t, work, 48*time.Hour, 1024)

	// Still being written to, deep inside, while the top looks ancient.
	out := filepath.Join(work, "inputs", "fresh.part")
	if err := os.WriteFile(out, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, _, err := SweepScratch(dir, ScratchMaxAge, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("removed %d — a build still writing output was treated as abandoned", n)
	}
}

// Nothing else in the directory is ours to delete.
func TestSweepTouchesNothingElse(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "someone-elses-data")
	mkTree(t, other, 48*time.Hour, 1024)
	loose := filepath.Join(dir, "varhub-not-a-dir")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := SweepScratch(dir, ScratchMaxAge, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{other, loose} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}
