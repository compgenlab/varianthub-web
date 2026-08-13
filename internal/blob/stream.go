package blob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// uploadPartSize and uploadConcurrency bound what a streaming upload holds in
// memory.
//
// The multipart uploader cannot seek a plain reader, so it buffers a whole part
// per worker before sending it: memory is partSize × concurrency, not partSize.
// Put uses 64 MB parts because it uploads a reference genome from a file the
// uploader *can* seek, where the buffer does not apply. Reusing that figure here
// would mean 320 MB resident per concurrent upload — on the request path, which
// is the exact failure this streaming exists to avoid.
//
// 8 × 4 is 32 MB per upload, and 8 MB parts still clear S3's 10,000-part ceiling
// by a wide margin: 80 GB, far past any input we accept.
const (
	uploadPartSize    = 8 * 1024 * 1024
	uploadConcurrency = 4
)

// PutReader streams r to either kind of storage, without holding it in memory.
//
// The counterpart to Put, which takes a path. A caller holding an open stream —
// an HTTP upload, a pipe, one object being rewritten into another — has no file
// to name, and spilling to a temp file just to name one is a copy of the whole
// input for nothing.
func PutReader(ctx context.Context, uri string, r io.Reader) error {
	if !IsS3(uri) {
		if err := os.MkdirAll(filepath.Dir(uri), 0o755); err != nil {
			return err
		}
		// Write-then-rename, as PutBytes does: a reader must never see a
		// half-written input. It matters more for a stream, because the window
		// in which the file is incomplete is as long as the upload.
		tmp := uri + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
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
	up := manager.NewUploader(c, func(u *manager.Uploader) {
		u.PartSize = uploadPartSize
		u.Concurrency = uploadConcurrency
		// Abort on a failed part rather than leaving them billable and, worse,
		// resumable into a mixture of two uploads.
		u.LeavePartsOnError = false
	})
	if _, err := up.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: r,
	}); err != nil {
		return fmt.Errorf("put %s: %w", uri, err)
	}
	return nil
}

// Open returns a stream over an object or file. The caller closes it.
//
// The counterpart to Get, which copies into a writer the caller already has.
// This is for a caller that wants to *parse* what is stored — feed it to a VCF
// reader, count lines, take the header — where materializing it first defeats
// the point.
func Open(ctx context.Context, uri string) (io.ReadCloser, error) {
	if !IsS3(uri) {
		return os.Open(uri)
	}
	bucket, key, err := splitURI(uri)
	if err != nil {
		return nil, err
	}
	c, err := clientFor(ctx, uri)
	if err != nil {
		return nil, err
	}
	out, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", uri, err)
	}
	return out.Body, nil
}

// Download streams an object or file to a local path, creating parent
// directories. Returns the bytes written.
//
// What a worker does with a job's input before handing it to the engine: the
// engine takes a filename, the input may be in a bucket, and nothing in between
// should hold it. Write-then-rename so a partial stage is never mistaken for a
// staged file.
func Download(ctx context.Context, uri, localPath string) (int64, error) {
	rc, err := Open(ctx, uri)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return 0, err
	}
	tmp := localPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, rc)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, fmt.Errorf("stage %s: %w", uri, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, localPath); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, nil
}
