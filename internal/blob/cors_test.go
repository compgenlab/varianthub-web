package blob

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// --- reading a bucket's rules ---

func TestARuleMustAllowGetAndNotJustTheOrigin(t *testing.T) {
	origin := "https://variants.example.org"
	cases := []struct {
		name  string
		rules []types.CORSRule
		want  bool
	}{
		{"the rule we write", []types.CORSRule{DownloadCORSRule([]string{origin})}, true},
		{"no rules at all", nil, false},
		{
			// The trap this exists for: a bucket set up for browser *uploads*
			// names the right origin and would read as configured, while every
			// download from it is still blocked.
			"the origin, but only for writing",
			[]types.CORSRule{{AllowedMethods: []string{"PUT", "POST"},
				AllowedOrigins: []string{origin}}},
			false,
		},
		{
			"GET, but for somebody else",
			[]types.CORSRule{{AllowedMethods: []string{"GET"},
				AllowedOrigins: []string{"https://elsewhere.example.org"}}},
			false,
		},
		{
			"spread over two rules, neither of which allows both",
			[]types.CORSRule{
				{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"https://other.example"}},
				{AllowedMethods: []string{"PUT"}, AllowedOrigins: []string{origin}},
			},
			false,
		},
		{
			"one of several rules allows it",
			[]types.CORSRule{
				{AllowedMethods: []string{"PUT"}, AllowedOrigins: []string{"https://other.example"}},
				{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{origin}},
			},
			true,
		},
		{"any origin", []types.CORSRule{{AllowedMethods: []string{"GET"},
			AllowedOrigins: []string{"*"}}}, true},
		{"lower-cased method", []types.CORSRule{{AllowedMethods: []string{"get"},
			AllowedOrigins: []string{origin}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := corsAllows(c.rules, origin); got != c.want {
				t.Errorf("corsAllows = %v, want %v", got, c.want)
			}
		})
	}
}

// S3 permits one wildcard inside an origin. Reading it literally would report a
// correctly configured bucket as broken and switch a working deployment to
// relaying every download — a performance regression with no visible cause.
func TestAWildcardOriginIsNotALiteral(t *testing.T) {
	cases := []struct {
		pattern, origin string
		want            bool
	}{
		{"https://*.example.org", "https://variants.example.org", true},
		{"https://*.example.org", "https://variants.example.com", false},
		{"https://*.example.org", "http://variants.example.org", false},
		{"*", "https://anything.at.all", true},
		{"https://variants.example.org", "https://variants.example.org", true},
		{"https://variants.example.org", "https://variants.example.org:8443", false},
		// The prefix and suffix must not be allowed to overlap into a match on
		// a string shorter than both.
		{"https://*.example.org", "https://.example.org", true},
		{"https://*.example.org", "https://exam", false},
	}
	for _, c := range cases {
		if got := originMatches(c.pattern, c.origin); got != c.want {
			t.Errorf("originMatches(%q, %q) = %v, want %v", c.pattern, c.origin, got, c.want)
		}
	}
}

// --- refusing to sign a link the browser cannot use ---

// A site declared public whose bucket is known to block the web origin must
// stop being redirected to. The link would be valid, correctly signed, and
// unreadable by the only client that asked for it.
func TestABlockedSiteMintsNoLink(t *testing.T) {
	withSites(t, Site{
		Name: "results", URI: "s3://results",
		Endpoint: "https://files.example.org", PublicEndpoint: true,
		Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})

	if _, ok, err := Presign(context.Background(), "s3://results/jobs/j1/result.vcf.gz",
		time.Minute, Disposition{}); err != nil || !ok {
		t.Fatalf("a public site minted no link before being blocked: ok=%v err=%v", ok, err)
	}

	BlockPresign("results", "its bucket does not allow GET from the web origin")
	t.Cleanup(func() {
		blockedMu.Lock()
		delete(blocked, "results")
		blockedMu.Unlock()
	})

	url, ok, err := Presign(context.Background(), "s3://results/jobs/j1/result.vcf.gz",
		time.Minute, Disposition{})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if ok {
		t.Errorf("a link was minted for a site whose bucket blocks the browser: %s", url)
	}
}

// --- against a live gateway ---

// The claim this whole file rests on, checked against a real object store
// rather than against our idea of one: with the rule applied, a cross-origin
// GET of a presigned URL comes back with the header a browser requires, and
// without it, it does not.
//
// Asserting on the response header rather than on the bucket's stored config is
// deliberate. A gateway can accept PutBucketCors and enforce nothing, and a test
// that read the rule back would pass against exactly that.
func TestABucketWithTheRuleAnswersACrossOriginRead(t *testing.T) {
	bucket := testBucket(t)
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	ctx := context.Background()

	site := Site{
		Name: "live", URI: bucket, Endpoint: endpoint, PublicEndpoint: true,
		Region: os.Getenv("AWS_REGION"),
		// From the environment, like every other live test here.
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	withSites(t, site)

	local := filepath.Join(t.TempDir(), "result.vcf.gz")
	if err := os.WriteFile(local, []byte("bgzf pretend"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bucket + "/cors-test/result.vcf.gz"
	if err := Put(ctx, local, key); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), key) })

	const origin = "https://variants.example.test"
	if err := PutCORS(ctx, site, []string{origin}); err != nil {
		// Skipping only for a gateway that does not implement the call. Any
		// other failure is ours, and skipping on it would turn a bug in this
		// package into a green run.
		if !unsupportedOperation(err) {
			t.Fatalf("PutCORS: %v", err)
		}
		t.Skipf("this gateway does not implement bucket CORS: %v", err)
	}
	t.Cleanup(func() {
		// Left as it was found is not possible — S3 has no rule-level delete —
		// so leave it permissive for whatever runs next rather than half-set.
		_ = PutCORS(context.Background(), site, []string{"*"})
	})

	missing, known, err := CheckCORS(ctx, site, []string{origin})
	if err != nil || !known {
		t.Fatalf("CheckCORS after applying the rule: known=%v err=%v", known, err)
	}
	if len(missing) > 0 {
		t.Fatalf("the rule was applied and CheckCORS still reports %v missing", missing)
	}

	url, ok, err := Presign(ctx, key, 5*time.Minute,
		Disposition{Filename: "variants-j1.vcf.gz", ContentType: "application/gzip"})
	if err != nil || !ok {
		t.Fatalf("presign: ok=%v err=%v", ok, err)
	}

	// The allowed origin: the browser is told it may read the response.
	if got := corsHeaderFor(t, url, origin); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q — a browser would refuse "+
			"to read this download", got, origin)
	}
	// And an origin the rule does not name gets nothing, so the rule is doing
	// something rather than the gateway allowing everything.
	if got := corsHeaderFor(t, url, "https://elsewhere.example.test"); got == "https://elsewhere.example.test" {
		t.Errorf("the gateway allowed an origin the rule does not name (%q); this test "+
			"would pass with no rule at all", got)
	}
}

// The other half: a bucket allowing only somebody else is reported as missing
// the origin, which is what makes serve stop minting links for it.
func TestABucketWithoutTheRuleIsReportedMissing(t *testing.T) {
	bucket := testBucket(t)
	ctx := context.Background()

	site := Site{
		Name: "live", URI: bucket, Endpoint: os.Getenv("AWS_ENDPOINT_URL"),
		PublicEndpoint: true, Region: os.Getenv("AWS_REGION"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	withSites(t, site)

	if err := PutCORS(ctx, site, []string{"https://somebody-else.example.test"}); err != nil {
		if !unsupportedOperation(err) {
			t.Fatalf("PutCORS: %v", err)
		}
		t.Skipf("this gateway does not implement bucket CORS: %v", err)
	}
	t.Cleanup(func() { _ = PutCORS(context.Background(), site, []string{"*"}) })

	const ours = "https://variants.example.test"
	missing, known, err := CheckCORS(ctx, site, []string{ours})
	if err != nil || !known {
		t.Fatalf("CheckCORS: known=%v err=%v", known, err)
	}
	if len(missing) != 1 || missing[0] != ours {
		t.Fatalf("missing = %v, want just %q", missing, ours)
	}

	// And that is what VerifyPublicSites acts on.
	notes := VerifyPublicSites(ctx, []string{ours})
	t.Cleanup(func() {
		blockedMu.Lock()
		delete(blocked, "live")
		blockedMu.Unlock()
	})
	if len(notes) == 0 {
		t.Fatal("VerifyPublicSites said nothing about a site whose bucket blocks the web origin")
	}
	if !strings.Contains(notes[0], "relayed") {
		t.Errorf("the note does not say what will happen instead: %q", notes[0])
	}
	if _, no := presignBlocked("live"); !no {
		t.Error("the site was reported but not blocked, so links are still being minted")
	}
}

// corsHeaderFor makes the request a browser makes when it follows the redirect,
// and returns what the store said about the origin.
func corsHeaderFor(t *testing.T, url, origin string) string {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch the presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the presigned URL answered %s", resp.Status)
	}
	return resp.Header.Get("Access-Control-Allow-Origin")
}

// unsupportedOperation reports the one failure these tests may skip on: a
// gateway that does not implement bucket CORS at all.
func unsupportedOperation(err error) bool {
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotImplemented", "MethodNotAllowed", "InvalidRequest":
			return true
		}
	}
	return false
}
