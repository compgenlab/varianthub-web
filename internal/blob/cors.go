package blob

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The bucket rule a download link needs, and noticing when it is missing.
//
// A presigned URL removes this service from the transfer, which is the whole
// point of it — but it does so by sending the caller somewhere else, and a
// browser will not read a cross-origin response unless the host it came from
// says the origin may have it. So a link that works perfectly from curl is dead
// from the web app, and the two symptoms arrive together: the API is healthy,
// every command-line client is fine, and the download button does nothing.
//
// That is a bad failure to leave to a deployment note, because nothing in the
// service is wrong. The bucket is a separate system with its own configuration,
// and the only way this can be sure is to go and ask it.
//
// So there are two halves here. PutCORS writes the rule, run once by whoever
// sets public_endpoint. CheckCORS reads it back at startup, and a public site
// whose bucket is *known* not to allow the app's origin stops minting links for
// the rest of the process — the download relays instead, which is exactly what
// the site would have done had it never been declared public. Slower and
// correct beats faster and broken.
//
// Known is the important word. A deployment may hold a key that can read and
// write objects but not read the bucket's configuration, and refusing to
// presign because the question could not be asked would break working
// installations to guard against a misconfiguration that may not exist. So an
// unanswerable check leaves presigning on and says so once in the log.

// corsMethods is what a download does and nothing more.
//
// GET fetches the object and HEAD is what a client checks a size with. Anything
// that writes is not something a browser should be doing against this bucket
// with no credential but a signed URL.
var corsMethods = []string{"GET", "HEAD"}

// corsExposed are the response headers the browser is allowed to read.
//
// Content-Disposition is the one that matters: the filename a download is saved
// under travels in it, and a browser that cannot read it names the file after
// the URL path — result.vcf.gz under a job id, rather than the name the export
// asked for.
var corsExposed = []string{"Content-Disposition", "Content-Length", "Content-Type", "ETag"}

// corsMaxAge is how long a browser may cache the preflight, in seconds.
const corsMaxAge = 3000

// DownloadCORSRule is the rule a public site's bucket needs.
//
// Named origins rather than "*". A signed URL is a bearer capability, so a
// wildcard would not leak anything that holding the URL does not already grant
// — but it would let any page that obtained one read the object's bytes through
// a script instead of merely linking to it, and there is no deployment here that
// needs that.
func DownloadCORSRule(origins []string) types.CORSRule {
	return types.CORSRule{
		AllowedMethods: append([]string(nil), corsMethods...),
		AllowedOrigins: append([]string(nil), origins...),
		AllowedHeaders: []string{"*"},
		ExposeHeaders:  append([]string(nil), corsExposed...),
		MaxAgeSeconds:  aws.Int32(corsMaxAge),
	}
}

// PutCORS applies the download rule to the bucket a site's URI names.
//
// It replaces the bucket's whole CORS configuration, because that is the only
// operation S3 has — there is no "add a rule". A bucket shared with something
// else that needs its own rule has to have both written together, which is why
// this is a command an operator runs rather than something done at startup.
func PutCORS(ctx context.Context, site Site, origins []string) error {
	if len(origins) == 0 {
		return errors.New("no origins to allow; set public_url or cors_origins")
	}
	bucket, err := bucketOf(site.URI)
	if err != nil {
		return err
	}
	// The site's own client, which is the one configured to reach it. Its
	// endpoint is the internal address; configuring a bucket is done from
	// inside the deployment even when the reading of it is not.
	c, err := clientFor(ctx, site.URI)
	if err != nil {
		return err
	}
	rule := DownloadCORSRule(origins)
	_, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket:            aws.String(bucket),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{rule}},
	})
	if err != nil {
		return fmt.Errorf("set CORS on %s: %w", bucket, err)
	}
	return nil
}

// CheckCORS reports which of origins the site's bucket does not allow to GET.
//
// known is false when the bucket could not be asked at all — no permission to
// read its configuration, or a gateway that does not implement the call. That is
// a different answer from "allows nothing", and the caller must treat it
// differently: one is a misconfiguration to act on, the other is a question
// this deployment cannot answer.
//
// A bucket with no CORS configuration at all answers NoSuchCORSConfiguration,
// which is known and allows nothing. That is the case this exists to catch.
func CheckCORS(ctx context.Context, site Site, origins []string) (missing []string, known bool, err error) {
	bucket, err := bucketOf(site.URI)
	if err != nil {
		return nil, false, err
	}
	c, err := clientFor(ctx, site.URI)
	if err != nil {
		return nil, false, err
	}
	out, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(bucket)})
	if err != nil {
		if noSuchCORS(err) {
			// Answered, and the answer is none.
			return append([]string(nil), origins...), true, nil
		}
		return nil, false, fmt.Errorf("read CORS on %s: %w", bucket, err)
	}
	for _, o := range origins {
		if !corsAllows(out.CORSRules, o) {
			missing = append(missing, o)
		}
	}
	return missing, true, nil
}

// bucketOf names the bucket a site's URI is in, prefix or not.
//
// splitURI is not usable here: it requires a key, because every other operation
// in this package acts on an object. CORS is configured on the bucket, and a
// site is ordinarily declared as a bare "s3://bucket" — so reusing splitURI
// rejected exactly the configuration this is written for.
func bucketOf(uri string) (string, error) {
	rest := strings.TrimPrefix(uri, "s3://")
	if rest == uri {
		return "", fmt.Errorf("%q is not an s3:// URI", uri)
	}
	bucket, _, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return "", fmt.Errorf("%q has no bucket", uri)
	}
	return bucket, nil
}

// noSuchCORS reports the "this bucket has no CORS configuration" answer.
//
// Matched on the code rather than the type because the gateways differ: AWS
// returns a modelled NoSuchCORSConfiguration, and an S3-compatible gateway may
// return the same code in a generic API error. Both mean the bucket answered.
func noSuchCORS(err error) bool {
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		return ae.ErrorCode() == "NoSuchCORSConfiguration"
	}
	return false
}

// corsAllows reports whether some rule lets origin GET.
//
// A rule has to permit both, which is easy to get wrong by reading only the
// origins: a bucket whose rule allows the web origin for PUT alone would look
// configured and still block every download.
func corsAllows(rules []types.CORSRule, origin string) bool {
	for _, r := range rules {
		if !hasMethod(r.AllowedMethods, "GET") {
			continue
		}
		for _, o := range r.AllowedOrigins {
			if originMatches(o, origin) {
				return true
			}
		}
	}
	return false
}

func hasMethod(methods []string, want string) bool {
	for _, m := range methods {
		if strings.EqualFold(m, want) {
			return true
		}
	}
	return false
}

// originMatches compares a configured origin against the app's.
//
// S3 allows one "*" wildcard inside an origin — https://*.example.org — as well
// as a bare "*" for any origin. Treating the wildcard form as a literal would
// report a correctly configured bucket as missing the rule and switch a working
// deployment to relaying.
func originMatches(pattern, origin string) bool {
	if pattern == "*" {
		return true
	}
	i := strings.Index(pattern, "*")
	if i < 0 {
		return strings.EqualFold(pattern, origin)
	}
	prefix, suffix := pattern[:i], pattern[i+1:]
	if len(origin) < len(prefix)+len(suffix) {
		return false
	}
	return strings.EqualFold(origin[:len(prefix)], prefix) &&
		strings.EqualFold(origin[len(origin)-len(suffix):], suffix)
}

// --- refusing to sign links a browser cannot use ---

var (
	blockedMu sync.RWMutex
	blocked   = map[string]string{} // site name -> why
)

// BlockPresign stops this process minting links for a site, with a reason.
//
// Keyed by site name, and the unnamed site — the one configured by environment
// variables rather than a [[s3]] block — is "", which is a real key here rather
// than a missing one.
func BlockPresign(siteName, reason string) {
	blockedMu.Lock()
	blocked[siteName] = reason
	blockedMu.Unlock()
}

// presignBlocked reports why a site's links are refused, if they are.
func presignBlocked(siteName string) (string, bool) {
	blockedMu.RLock()
	defer blockedMu.RUnlock()
	r, ok := blocked[siteName]
	return r, ok
}

// VerifyPublicSites checks every site declared publicly reachable and blocks the
// ones whose bucket will not let the app's origin read a download.
//
// Called once at startup by the process that serves downloads. Returns the
// human-readable findings in the order the sites were declared, so the caller
// logs them rather than this reaching for a logger of its own.
//
// A site that cannot be asked is left alone and reported; see the note at the
// top of this file for why that direction is the safe one.
func VerifyPublicSites(ctx context.Context, origins []string) []string {
	sitesMu.RLock()
	declared := append([]Site(nil), sites...)
	sitesMu.RUnlock()

	if len(origins) == 0 {
		return nil
	}
	var notes []string
	for _, s := range declared {
		if !s.PublicEndpoint {
			continue
		}
		missing, known, err := CheckCORS(ctx, s, origins)
		switch {
		case err != nil && !known:
			notes = append(notes, fmt.Sprintf(
				"site %q: could not read its bucket's CORS rules, so download links "+
					"are still being minted and may not work in a browser: %v", s.Name, err))
		case len(missing) > 0:
			sort.Strings(missing)
			reason := fmt.Sprintf("its bucket does not allow GET from %s",
				strings.Join(missing, ", "))
			BlockPresign(s.Name, reason)
			notes = append(notes, fmt.Sprintf(
				"site %q: %s, so downloads will be relayed through this server "+
					"instead of redirected. Fix with: varianthub-web s3 cors --apply",
				s.Name, reason))
		}
	}
	return notes
}
