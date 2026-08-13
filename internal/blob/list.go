package blob

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object is one stored file: where it is, and when it was last written.
//
// The time is what makes a sweep safe. An object younger than the window in
// which its owning row is written is not an orphan, it is an upload in
// progress, and deleting it takes the input out from under a job somebody just
// submitted.
type Object struct {
	URI     string
	Size    int64
	ModTime time.Time
}

// List returns every object under a prefix, over either kind of storage.
//
// Recursive and flat: an object store has no directories, only keys that happen
// to contain slashes, so presenting the filesystem side any other way would make
// the two behave differently for the one caller that has to treat them alike.
func List(ctx context.Context, prefix string) ([]Object, error) {
	if !IsS3(prefix) {
		return listDir(prefix)
	}
	bucket, key, err := splitURI(prefix)
	if err != nil {
		return nil, err
	}
	c, err := clientFor(ctx, prefix)
	if err != nil {
		return nil, err
	}

	var out []Object
	// Paginated because a bucket holding a week of jobs is well past the
	// thousand keys one response carries, and a sweep that silently saw the
	// first page would leave everything after it forever.
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(key),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			// Rebuilt from the bucket and the returned key, not by patching
			// the prefix that was passed in: a key is absolute within its
			// bucket, and reconstructing from anything else means two spellings
			// of one location.
			obj := Object{URI: "s3://" + bucket + "/" + aws.ToString(o.Key)}
			if o.Size != nil {
				obj.Size = *o.Size
			}
			if o.LastModified != nil {
				obj.ModTime = *o.LastModified
			}
			out = append(out, obj)
		}
	}
	return out, nil
}

// listDir is List over a filesystem prefix.
//
// A missing directory is an empty listing rather than an error: a deployment
// that has not yet accepted an upload has no jobs directory, and a sweep should
// find nothing rather than fail.
func listDir(root string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// Vanished between the walk and the stat, which is exactly what a
			// concurrent sweep or a finishing job looks like. Not this call's
			// problem to report.
			return nil //nolint:nilerr
		}
		out = append(out, Object{URI: p, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}
