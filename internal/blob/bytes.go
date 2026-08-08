package blob

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// IsS3 reports whether a locator names an object rather than a file.
func IsS3(uri string) bool { return strings.HasPrefix(uri, "s3://") }

// PutBytes writes a small object to either kind of storage.
//
// Separate from Put, which streams a local file through a multipart uploader
// because a reference genome is most of a gigabyte. These are helper scripts of
// a few KB held in memory, and a local destination is a file, not an upload.
func PutBytes(ctx context.Context, uri string, data []byte) error {
	if !IsS3(uri) {
		if err := os.MkdirAll(filepath.Dir(uri), 0o755); err != nil {
			return err
		}
		// Write-then-rename, so a reader never sees a half-written asset. It
		// matters more here than usual: the name is a content digest, so a
		// truncated file would be found under a digest it does not have.
		tmp := uri + ".part"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, uri)
	}
	bucket, key, err := splitURI(uri)
	if err != nil {
		return err
	}
	c, err := clientFor(ctx, uri)
	if err != nil {
		return err
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
	}); err != nil {
		return fmt.Errorf("put %s: %w", uri, err)
	}
	return nil
}

// GetBytes reads a small object from either kind of storage.
func GetBytes(ctx context.Context, uri string) ([]byte, error) {
	if !IsS3(uri) {
		return os.ReadFile(uri)
	}
	var buf bytes.Buffer
	if _, err := Get(ctx, uri, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
