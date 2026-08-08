package blob

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Health is the result of probing one storage location.
type Health struct {
	OK      bool          `json:"ok"`
	Latency time.Duration `json:"-"`
	Millis  int64         `json:"latency_ms"`
	Error   string        `json:"error,omitempty"`
	// Hint distinguishes failures an operator acts on differently. The two that
	// matter here look identical in a raw SDK error and have nothing in common:
	// an endpoint that is down, and credentials that are wrong.
	Hint string `json:"hint,omitempty"`
}

// Check probes a storage location and reports whether it can be reached.
//
// A cheap, read-only operation — one HEAD against the bucket. Enough to
// distinguish reachable from not, without writing anything or listing a bucket
// that may hold millions of objects.
//
// Worth doing at all because the alternative is finding out from a job. An
// object store that stopped answering surfaced as a provisioning failure whose
// message was an SDK retry trace, hours into a run, on a page that had no
// opinion about whether the store was up.
func Check(ctx context.Context, uri string) Health {
	start := time.Now()
	done := func(err error, hint string) Health {
		h := Health{Latency: time.Since(start)}
		h.Millis = h.Latency.Milliseconds()
		if err == nil {
			h.OK = true
			return h
		}
		h.Error = err.Error()
		h.Hint = hint
		return h
	}

	if !IsS3(uri) {
		// A filesystem location: reachable means the directory exists and can be
		// written to. A path target the worker cannot see is a download that
		// fails at job time rather than at configuration time, which is the same
		// class of surprise this exists to remove.
		fi, err := os.Stat(uri)
		if err != nil {
			return done(err, "the path does not exist on this host")
		}
		if !fi.IsDir() {
			return done(fmt.Errorf("%s is not a directory", uri), "")
		}
		probe := filepath.Join(uri, ".varianthub-write-probe")
		if err := os.WriteFile(probe, nil, 0o644); err != nil {
			return done(err, "the path exists but is not writable by this process")
		}
		os.Remove(probe)
		return done(nil, "")
	}

	bucket, _, err := splitURI(uri)
	if err != nil {
		return done(err, "")
	}
	cli, err := clientFor(ctx, uri)
	if err != nil {
		return done(err, "no client for this location — check its [[s3]] site")
	}
	// Bounded: a probe that hangs is a probe nobody waits for, and an
	// unreachable endpoint is exactly the case where a default timeout is long.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err = cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket})
	return done(err, hintFor(err))
}

// hintFor turns an SDK error into the distinction an operator acts on.
//
// "Connection refused" and "signature does not match" arrive as the same kind of
// value and mean entirely different things: one is somebody else's outage to
// wait out, the other is a credential to fix. Saying which saves the reading.
func hintFor(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"),
		strings.Contains(s, "no such host"),
		strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "context deadline exceeded"):
		return "the endpoint did not answer — the store is unreachable, not misconfigured. " +
			"Credentials were never presented, so this is not an authentication problem."
	case strings.Contains(s, "SignatureDoesNotMatch"),
		strings.Contains(s, "InvalidAccessKeyId"),
		strings.Contains(s, "AccessDenied"),
		strings.Contains(s, "403"):
		return "the endpoint answered and rejected the credentials — check access_key " +
			"and secret_key for this site."
	case strings.Contains(s, "NoSuchBucket"), strings.Contains(s, "404"):
		return "the endpoint answered; the bucket does not exist."
	}
	return ""
}
