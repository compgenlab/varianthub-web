// Package blob is the narrow object-store surface this service needs: read one
// object, write one, ask whether one is there, and copy between locations.
//
// Separate from the worker because both processes need it — the API uploads a
// source's assets when it registers one, and the worker moves and fetches
// files — and because varhub's own object-store code serves a different
// purpose: it provisions annotation data, while this handles the artifacts the
// service itself keeps.
package blob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
func client(ctx context.Context) (*s3.Client, error) {
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
func splitURI(uri string) (bucket, key string, err error) {
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
func Get(ctx context.Context, uri string, w io.Writer) (int64, error) {
	bucket, key, err := splitURI(uri)
	if err != nil {
		return 0, err
	}
	c, err := client(ctx)
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
func Put(ctx context.Context, localPath, uri string) error {
	bucket, key, err := splitURI(uri)
	if err != nil {
		return err
	}
	c, err := client(ctx)
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
func Exists(ctx context.Context, uri string) bool {
	bucket, key, err := splitURI(uri)
	if err != nil {
		return false
	}
	c, err := client(ctx)
	if err != nil {
		return false
	}
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	return err == nil
}

// Transfer copies one file between locations, either of which may be a
// filesystem path or an object locator.
//
// Copy, never move: the caller deletes the original only after the copy is
// confirmed. A move that deletes first turns a transient network failure into
// lost data, and these are the files a deployment cannot cheaply re-fetch.
func Transfer(ctx context.Context, src, dst string) (int64, error) {
	srcS3 := strings.HasPrefix(src, "s3://")
	dstS3 := strings.HasPrefix(dst, "s3://")

	switch {
	case !srcS3 && !dstS3:
		if err := CopyTo(ctx, src, dst); err != nil {
			return 0, err
		}
		fi, err := os.Stat(dst)
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil

	case !srcS3 && dstS3:
		fi, err := os.Stat(src)
		if err != nil {
			return 0, err
		}
		if err := Put(ctx, src, dst); err != nil {
			return 0, err
		}
		return fi.Size(), nil

	case srcS3 && !dstS3:
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		tmp := dst + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			return 0, err
		}
		n, err := Get(ctx, src, f)
		f.Close()
		if err != nil {
			os.Remove(tmp)
			return 0, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return 0, err
		}
		return n, nil

	default:
		// Bucket to bucket. Streamed through this process rather than served by
		// a server-side copy, because the two ends may be different endpoints —
		// versitygw and AWS, say — and a CopyObject cannot cross that.
		tmp, err := os.CreateTemp("", "varhub-move-*")
		if err != nil {
			return 0, err
		}
		defer os.Remove(tmp.Name())
		n, err := Get(ctx, src, tmp)
		if err != nil {
			tmp.Close()
			return 0, err
		}
		if err := tmp.Close(); err != nil {
			return 0, err
		}
		if err := Put(ctx, tmp.Name(), dst); err != nil {
			return 0, err
		}
		return n, nil
	}
}

// Remove deletes a file at a path or an object locator, tolerating absence.
func Remove(ctx context.Context, loc string) error {
	if !strings.HasPrefix(loc, "s3://") {
		if err := os.Remove(loc); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	bucket, key, err := splitURI(loc)
	if err != nil {
		return err
	}
	c, err := client(ctx)
	if err != nil {
		return err
	}
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	return err
}

// CopyTo writes a local file to a destination that may be a filesystem path or
// an object locator.
func CopyTo(ctx context.Context, src, dst string) error {
	if strings.HasPrefix(dst, "s3://") {
		return Put(ctx, src, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
