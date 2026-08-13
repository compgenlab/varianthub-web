package blob

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func withSites(t *testing.T, s ...Site) {
	t.Helper()
	RegisterSites(s)
	t.Cleanup(func() { RegisterSites(nil) })
}

// A gateway nobody has vouched for yields no link.
//
// The client that reads objects is configured with the address this service
// uses, which in a container deployment is an in-cluster name. Signing against
// it produces a URL that is correctly signed and unreachable, and it fails at
// the caller as a DNS error that says nothing about why. Refusing costs
// bandwidth; guessing costs every download at once.
func TestAPrivateGatewayMintsNoLink(t *testing.T) {
	withSites(t, Site{
		Name: "internal", URI: "s3://results",
		Endpoint: "http://s3:7070", Region: "us-east-1",
		AccessKey: "k", SecretKey: "s",
	})

	url, ok, err := Presign(context.Background(), "s3://results/jobs/j1/result.vcf.gz",
		time.Minute, Disposition{})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if ok {
		t.Errorf("a link was minted against an endpoint declared only for internal "+
			"use: %s", url)
	}
}

// Declaring the endpoint reachable is what turns it on, and the link names it.
func TestAPublicEndpointMintsALinkAgainstItself(t *testing.T) {
	withSites(t, Site{
		Name: "results", URI: "s3://results",
		Endpoint: "https://files.example.org", PublicEndpoint: true,
		Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})

	raw, ok, err := Presign(context.Background(), "s3://results/jobs/j1/result.vcf.gz",
		15*time.Minute, Disposition{Filename: "variants-j1.vcf.gz", ContentType: "application/gzip"})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if !ok {
		t.Fatal("no link was minted for a site with a public endpoint")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the minted link does not parse: %v", err)
	}
	if u.Host != "files.example.org" {
		t.Errorf("link points at %q, not the endpoint declared reachable", u.Host)
	}
	if !strings.Contains(u.Path, "jobs/j1/result.vcf.gz") {
		t.Errorf("link does not name the object: %s", u.Path)
	}
	// Signed, and therefore a capability rather than a plain request to a
	// private object.
	q := u.Query()
	for _, k := range []string{"X-Amz-Signature", "X-Amz-Credential", "X-Amz-Expires"} {
		if q.Get(k) == "" {
			t.Errorf("the link carries no %s; it is not signed", k)
		}
	}
	// The name the caller saves it under travels with the link, because the
	// object's own key is result.vcf.gz under a job prefix and that is not a
	// filename anybody wants twice in their downloads folder.
	if cd := q.Get("response-content-disposition"); !strings.Contains(cd, "variants-j1.vcf.gz") {
		t.Errorf("response-content-disposition = %q", cd)
	}
}

// A local path is not something to sign. False, and not an error: a deployment
// storing results on a filesystem is a supported one, and it simply relays.
func TestALocalPathMintsNoLink(t *testing.T) {
	_, ok, err := Presign(context.Background(), "/var/lib/varianthub/jobs/j1/result.vcf.gz",
		time.Minute, Disposition{})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if ok {
		t.Error("a filesystem path was presigned")
	}
}

// Re-registering sites drops the cached signing clients.
//
// They are keyed by site name, so without this a deployment that changed a
// site's public endpoint would keep signing links against the old host — and
// the links would look perfectly valid.
func TestRegisteringSitesAgainForgetsTheOldSigner(t *testing.T) {
	const uri = "s3://results/jobs/j1/result.vcf.gz"
	withSites(t, Site{
		Name: "results", URI: "s3://results",
		Endpoint: "https://old.example.org", PublicEndpoint: true, Region: "us-east-1",
		AccessKey: "k", SecretKey: "s",
	})
	if _, ok, err := Presign(context.Background(), uri, time.Minute, Disposition{}); err != nil || !ok {
		t.Fatalf("first presign: ok=%v err=%v", ok, err)
	}

	withSites(t, Site{
		Name: "results", URI: "s3://results",
		Endpoint: "https://new.example.org", PublicEndpoint: true, Region: "us-east-1",
		AccessKey: "k", SecretKey: "s",
	})
	raw, ok, err := Presign(context.Background(), uri, time.Minute, Disposition{})
	if err != nil || !ok {
		t.Fatalf("second presign: ok=%v err=%v", ok, err)
	}
	if u, _ := url.Parse(raw); u.Host != "new.example.org" {
		t.Errorf("link points at %q after the site was re-registered", u.Host)
	}
}
