package blob

import (
	"context"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Per-site S3 clients.
//
// One process-wide client from the environment cannot reach two targets with
// different credentials — a gateway on the cluster and a bucket at a provider,
// say — and a deployment with both is the ordinary case rather than an exotic
// one. A site declared in config carries its own endpoint and keys, and the
// client used for a URI is chosen by which site's prefix it falls under.
//
// Sites are registered once at startup. Nothing here reads a file: config owns
// that, and this owns talking to the store.

// Site is an object-storage target and what to present to it.
type Site struct {
	Name      string
	URI       string
	Endpoint string
	// PublicEndpoint says Endpoint is reachable by a caller outside the
	// deployment, and so may be signed into a download link. False — the
	// default — means no link is minted for this site and results are relayed
	// through the API instead. See presignClientFor.
	PublicEndpoint bool
	Region         string
	AccessKey      string
	SecretKey      string
	// Default marks the site an annotation job's credentials come from when
	// several are declared.
	Default bool
}

var (
	sitesMu sync.RWMutex
	sites   []Site
	clients = map[string]*s3.Client{}
)

// RegisterSites replaces the known sites. Called once, at startup.
//
// Both client caches are dropped, not just the reading one. They are keyed by
// site name, so a re-registration that changed a site's endpoint or keys would
// otherwise keep handing out a client built from the old ones — and for the
// presigning cache that means signing links against a host the deployment has
// stopped using.
func RegisterSites(s []Site) {
	sitesMu.Lock()
	sites = append([]Site(nil), s...)
	clients = map[string]*s3.Client{}
	sitesMu.Unlock()

	// Its own lock, taken after the other is released: two locks held at once in
	// two orders is how this deadlocks later, and nothing needs them together.
	presignMu.Lock()
	presignClients = map[string]*s3.Client{}
	presignMu.Unlock()
}

// siteFor returns the declared site a URI belongs to.
//
// Longest prefix wins, so a site at s3://bucket/annotations is chosen over one
// at s3://bucket for an object under the former. Without that the more general
// site would capture everything beneath it and its credentials would be used
// for a target it may not reach.
func siteFor(uri string) (Site, bool) {
	sitesMu.RLock()
	defer sitesMu.RUnlock()

	best, bestLen := Site{}, -1
	for _, s := range sites {
		p := strings.TrimRight(s.URI, "/")
		if p == "" {
			continue
		}
		if uri == p || strings.HasPrefix(uri, p+"/") {
			if len(p) > bestLen {
				best, bestLen = s, len(p)
			}
		}
	}
	return best, bestLen >= 0
}

// clientFor returns the client for a URI: the declared site's when one covers
// it, otherwise the environment's.
//
// Falling back rather than failing keeps a deployment that never declared a
// site working exactly as before, which is what every existing installation is.
func clientFor(ctx context.Context, uri string) (*s3.Client, error) {
	site, ok := siteFor(uri)
	if !ok {
		return client(ctx)
	}

	sitesMu.RLock()
	c := clients[site.Name]
	sitesMu.RUnlock()
	if c != nil {
		return c, nil
	}

	opts := []func(*awsconfig.LoadOptions) error{}
	if site.Region != "" {
		opts = append(opts, awsconfig.WithRegion(site.Region))
	}
	// Keys only when both are given. One without the other is a
	// half-filled config, and presenting it would fail in a way that reads as
	// a permission problem rather than a missing setting.
	if site.AccessKey != "" && site.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(site.AccessKey, site.SecretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // any value signs; gateways ignore it
	}
	c = s3.NewFromConfig(cfg, func(o *s3.Options) {
		if site.Endpoint != "" {
			o.BaseEndpoint = aws.String(site.Endpoint)
			// Virtual-host addressing needs wildcard DNS for the bucket, which
			// a local gateway does not have.
			o.UsePathStyle = true
		}
	})

	sitesMu.Lock()
	clients[site.Name] = c
	sitesMu.Unlock()
	return c, nil
}

// EnvFor returns the AWS environment a subprocess needs to reach a URI.
//
// varhub reads credentials from the environment — it is a separate program with
// its own SDK — so a per-site credential has to be handed to it that way. The
// worker sets these when it execs the CLI for a job.
//
// Empty when no declared site covers the URI: the process environment already
// carries whatever that case needs, and overriding it with nothing would break
// an installation that relies on an instance role.
func EnvFor(uri string) []string {
	site, ok := siteFor(uri)
	if !ok {
		return nil
	}
	var env []string
	if site.AccessKey != "" && site.SecretKey != "" {
		env = append(env,
			"AWS_ACCESS_KEY_ID="+site.AccessKey,
			"AWS_SECRET_ACCESS_KEY="+site.SecretKey)
	}
	if site.Region != "" {
		env = append(env, "AWS_REGION="+site.Region)
	}
	if site.Endpoint != "" {
		env = append(env, "AWS_ENDPOINT_URL="+site.Endpoint)
	}
	return env
}

// DefaultEnv returns the AWS environment for the only declared site, or for the
// one marked default when there are several.
//
// Annotation reads from wherever a snapshot's sources happen to live, which the
// runner does not know — and could not act on if it did, since the environment
// it hands the CLI has room for one credential set. With a single site, which
// is what a deployment has, this is exactly right. With several it is the
// default one, and a source on another site with different keys would not be
// reachable from an annotation job.
//
// Empty when no site is declared, leaving the process environment as it was.
func DefaultEnv() []string {
	sitesMu.RLock()
	if len(sites) == 0 {
		sitesMu.RUnlock()
		return nil
	}
	pick := sites[0]
	for _, s := range sites {
		if s.Default {
			pick = s
			break
		}
	}
	sitesMu.RUnlock()
	// Outside the lock: EnvFor takes it again, and RWMutex is not reentrant.
	return EnvFor(pick.URI)
}
