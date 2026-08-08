package blob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The distinction the hint exists for. These two arrive as the same kind of
// value and mean entirely different things: wait for someone else to fix it, or
// go fix your own config. Getting them the wrong way round sends an operator to
// the credentials when the store is simply down — which is exactly what
// happened, and what prompted this.
func TestHintSeparatesUnreachableFromRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // substring the hint must contain
	}{
		{"refused", errors.New(`dial tcp 149.165.158.2:8001: connect: connection refused`), "unreachable"},
		{"dns", errors.New(`lookup nope.example: no such host`), "unreachable"},
		{"timeout", errors.New(`context deadline exceeded`), "unreachable"},
		{"bad key", errors.New(`api error InvalidAccessKeyId: not found`), "credentials"},
		{"bad signature", errors.New(`api error SignatureDoesNotMatch: nope`), "credentials"},
		{"denied", errors.New(`api error AccessDenied: forbidden`), "credentials"},
		{"no bucket", errors.New(`api error NoSuchBucket: missing`), "bucket does not exist"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hintFor(c.err)
			if !strings.Contains(got, c.want) {
				t.Errorf("hintFor(%v) = %q, want it to mention %q", c.err, got, c.want)
			}
		})
	}
	if hintFor(nil) != "" {
		t.Error("a nil error produced a hint")
	}
	// An unrecognised error gets no hint rather than a wrong one: saying
	// nothing beats sending someone to the wrong place.
	if got := hintFor(errors.New("something else entirely")); got != "" {
		t.Errorf("unrecognised error got hint %q, want none", got)
	}
}

// A filesystem location is reachable when it exists and can be written to.
// "Exists" alone is not enough — a path the worker can see but not write is a
// download that fails at job time, which is the surprise this removes.
func TestCheckFilesystemLocation(t *testing.T) {
	ctx := context.Background()

	dir := t.TempDir()
	if h := Check(ctx, dir); !h.OK {
		t.Errorf("a writable directory reported unhealthy: %s", h.Error)
	}

	missing := filepath.Join(dir, "not-here")
	if h := Check(ctx, missing); h.OK {
		t.Error("a missing directory reported healthy")
	} else if !strings.Contains(h.Hint, "does not exist") {
		t.Errorf("hint = %q, want it to say the path does not exist", h.Hint)
	}

	// A file where a directory should be.
	f := filepath.Join(dir, "a-file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := Check(ctx, f); h.OK {
		t.Error("a regular file reported healthy as a storage location")
	}
}

// The probe must not leave anything behind.
func TestCheckLeavesNoProbeFile(t *testing.T) {
	dir := t.TempDir()
	if h := Check(context.Background(), dir); !h.OK {
		t.Fatalf("unhealthy: %s", h.Error)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("probe left files behind: %v", names)
	}
}
