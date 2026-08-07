package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object-store access for references.
//
// varhub owns the object store for source data, and this is deliberately not a
// second copy of that: it is the narrow pair of operations a reference needs —
// read one object, write one object — against the same endpoint and the same
// ambient credentials. A reference is not a source, has no manifest, and is
// fetched by a job that never invokes varhub, so routing it through the CLI
// would mean inventing a subcommand for two calls.

var (
	s3Once   sync.Once
	s3Client *s3.Client
	s3Err    error
)

// s3c builds the S3 client once, from the standard credential chain.
//
// Lazy, because an installation with no object store must never see a
// credential error from a code path it does not use.
func s3c(ctx context.Context) (*s3.Client, error) {
	s3Once.Do(func() {
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			s3Err = fmt.Errorf("aws config: %w", err)
			return
		}
		if cfg.Region == "" {
			cfg.Region = "us-east-1" // any value signs; gateways ignore it
		}
		endpoint := firstEnv("AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL")
		s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			if endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
				// Virtual-host addressing needs wildcard DNS for the bucket,
				// which a local gateway does not have.
				o.UsePathStyle = true
			}
		})
	})
	return s3Client, s3Err
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// splitS3 parses "s3://bucket/key" into its parts.
func splitS3(uri string) (bucket, key string, err error) {
	rest := strings.TrimPrefix(uri, "s3://")
	if rest == uri {
		return "", "", fmt.Errorf("%q is not an s3:// URI", uri)
	}
	bucket, key, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("%q has no bucket", uri)
	}
	if key == "" {
		return "", "", fmt.Errorf("%q has no key", uri)
	}
	return bucket, key, nil
}

// s3Get streams an object into w.
func s3Get(ctx context.Context, uri string, w io.Writer) (int64, error) {
	bucket, key, err := splitS3(uri)
	if err != nil {
		return 0, err
	}
	c, err := s3c(ctx)
	if err != nil {
		return 0, err
	}
	out, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("get %s: %w", uri, err)
	}
	defer out.Body.Close()
	return io.Copy(w, out.Body)
}

// s3Put uploads a local file, using a multipart upload when it is large enough
// to need one — a reference is most of a gigabyte, so single PutObject is not
// viable.
func s3Put(ctx context.Context, localPath, uri string) error {
	bucket, key, err := splitS3(uri)
	if err != nil {
		return err
	}
	c, err := s3c(ctx)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	up := manager.NewUploader(c, func(u *manager.Uploader) {
		u.PartSize = 64 * 1024 * 1024
		// Abort on a failed part. Leaving them would bill for them and, worse,
		// a resumed run could complete an upload from a half-written mixture.
		u.LeavePartsOnError = false
	})
	if _, err := up.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: f,
	}); err != nil {
		return fmt.Errorf("put %s: %w", uri, err)
	}
	return nil
}

// s3Exists reports whether an object is there.
func s3Exists(ctx context.Context, uri string) bool {
	bucket, key, err := splitS3(uri)
	if err != nil {
		return false
	}
	c, err := s3c(ctx)
	if err != nil {
		return false
	}
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	return err == nil
}
