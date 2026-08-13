package blob

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Handing a caller a link to fetch an object with, instead of relaying it.
//
// A finished annotation is a file in a bucket. Streaming it through this service
// means every byte of a chromosome's worth of VCF is read out of the store,
// through the API process and out again — paid for twice, held open for as long
// as the client is slow, and multiplied by however many people are downloading
// at once. A presigned URL removes this service from the transfer entirely.
//
// What it costs is that the link is a bearer capability: whoever holds it can
// fetch that object until it expires, with no further check. So it is minted
// only after the caller's entitlement to the job has been established, it names
// one object, it is read-only, and it is short-lived.

// PresignTTL is how long a minted link stays good.
//
// Long enough for a slow client to start a large transfer — the clock is on
// starting the request, not on finishing it, so a download already under way is
// not cut off — and short enough that a link leaked into a shell history or a CI
// log stops working the same afternoon.
const PresignTTL = 15 * time.Minute

// Disposition is what the store should say the object is when it serves it.
//
// Set through the presigned URL rather than left to the object's stored
// metadata, because the name a download should have belongs to the request: the
// object is result.vcf.gz under a job's prefix, and what the caller wants saved
// is variants-<job>.vcf.gz.
type Disposition struct {
	Filename    string
	ContentType string
}

// Presign returns a URL that fetches uri directly from the object store.
//
// ok is false when no link can be made, which is an ordinary answer and not an
// error: the object is on a local filesystem, or the store is reachable only
// from inside the deployment. The caller streams it instead.
func Presign(ctx context.Context, uri string, ttl time.Duration, d Disposition) (string, bool, error) {
	if !IsS3(uri) {
		return "", false, nil
	}
	bucket, key, err := splitURI(uri)
	if err != nil {
		return "", false, err
	}
	c, ok, err := presignClientFor(ctx, uri)
	if err != nil || !ok {
		return "", false, err
	}
	if ttl <= 0 {
		ttl = PresignTTL
	}

	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if d.Filename != "" {
		in.ResponseContentDisposition = aws.String(
			`attachment; filename="` + sanitizeFilename(d.Filename) + `"`)
	}
	if d.ContentType != "" {
		in.ResponseContentType = aws.String(d.ContentType)
	}
	req, err := s3.NewPresignClient(c).PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", false, fmt.Errorf("presign %s: %w", uri, err)
	}
	return req.URL, true, nil
}

// sanitizeFilename keeps a filename from ending the header value it is quoted
// inside. A job id is hex and a format is from a fixed set, so this guards
// against a future caller rather than a present one.
func sanitizeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', '\\', '\r', '\n', ';':
			return '_'
		}
		return r
	}, s)
}

var (
	presignMu      sync.Mutex
	presignClients = map[string]*s3.Client{}
)

// presignClientFor builds a client whose signed URLs name a host the caller can
// actually reach.
//
// This is the whole difficulty. The client that reads and writes objects is
// configured with the endpoint *this service* uses, which in a container
// deployment is an in-cluster name — http://s3:7070 — that resolves to nothing
// from a laptop. A URL signed against it is correctly signed and useless, and it
// fails at the client with a DNS error that says nothing about why.
//
// So a link is minted only against an endpoint someone has said is externally
// reachable:
//
//   - the site's public_endpoint, or VHW_S3_PUBLIC_ENDPOINT;
//   - failing that, no custom endpoint at all, which means AWS itself and is
//     public by construction.
//
// A gateway with no public endpoint declared yields no link, and the download
// streams through this service exactly as it did before. Refusing is the safe
// direction: a missing setting costs bandwidth, while guessing costs every
// download on the deployment at once, for a reason that looks like a network
// fault rather than a configuration one.
func presignClientFor(ctx context.Context, uri string) (*s3.Client, bool, error) {
	site, declared := siteFor(uri)

	public := strings.TrimSpace(site.PublicEndpoint)
	if public == "" {
		public = firstEnv("VHW_S3_PUBLIC_ENDPOINT")
	}
	internal := site.Endpoint
	if !declared {
		internal = firstEnv("AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL")
	}
	if public == "" && internal != "" {
		return nil, false, nil // a gateway nobody has vouched for
	}

	cacheKey := site.Name + "\x00" + public
	presignMu.Lock()
	c := presignClients[cacheKey]
	presignMu.Unlock()
	if c != nil {
		return c, true, nil
	}

	var opts []func(*awsconfig.LoadOptions) error
	if site.Region != "" {
		opts = append(opts, awsconfig.WithRegion(site.Region))
	}
	// Signing needs a static key, not an anonymous client: an unsigned URL is
	// not a capability, it is a plain request to a private object.
	if site.AccessKey != "" && site.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(site.AccessKey, site.SecretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, false, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	c = s3.NewFromConfig(cfg, func(o *s3.Options) {
		if public != "" {
			o.BaseEndpoint = aws.String(public)
			o.UsePathStyle = true
		}
	})

	presignMu.Lock()
	presignClients[cacheKey] = c
	presignMu.Unlock()
	return c, true, nil
}
