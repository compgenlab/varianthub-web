// Package assetblob keeps source helper files in the same storage as the data
// and tool caches, addressed by the SHA-256 of their content.
//
// It sits between the catalog, which holds the index and knows nothing about
// buckets, and blob, which moves bytes and knows nothing about sources.
package assetblob

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// Prefix is where assets live under a storage location.
const Prefix = catalog.AssetPrefix

// Store resolves the storage location at use, not at construction.
//
// An administrator can add a location or change the default while the process
// is running, and an asset written after that should go where the data goes.
type Store struct {
	cat *catalog.Store
}

// New returns a Store that reads its destination from the catalog.
func New(cat *catalog.Store) *Store { return &Store{cat: cat} }

// uriFor composes the locator for a digest under the asset location.
func (s *Store) uriFor(ctx context.Context, digest string) (string, error) {
	if err := validDigest(digest); err != nil {
		return "", err
	}
	locs, err := s.cat.ListStorage(ctx)
	if err != nil {
		return "", err
	}
	loc, ok := catalog.AssetStorage(locs)
	if !ok {
		return "", errors.New("no usable storage location is configured")
	}
	return strings.TrimRight(loc.URI, "/") + "/" + catalog.AssetPrefix + "/" + digest, nil
}

// validDigest rejects anything that is not a plain lowercase SHA-256.
//
// The digest becomes a path component, so a value carrying a slash or a ".."
// would write outside the asset prefix — and for a local storage location that
// means anywhere the process can write.
func validDigest(d string) error {
	if len(d) != 64 {
		return fmt.Errorf("asset digest %q is not a SHA-256", d)
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("asset digest %q is not lowercase hex", d)
		}
	}
	return nil
}

// PutAsset stores content under its digest, skipping the write when the object
// already there holds the same bytes.
//
// The skip is what makes the digest worth using as a name: re-registering a
// source runs PutAssets again, and every settings change is a re-registration,
// so content that cannot have changed should not be uploaded again.
//
// It checks rather than assuming, though. Trusting the mere presence of an
// object would mean a corrupted one is never repaired — and re-registering the
// source is precisely what someone would try. Assets are a few KB, so reading
// one back costs nothing next to being permanently stuck with bytes the read
// side is right to refuse.
func (s *Store) PutAsset(ctx context.Context, digest string, content []byte) error {
	if err := catalog.VerifyAsset(digest, content); err != nil {
		return err
	}
	uri, err := s.uriFor(ctx, digest)
	if err != nil {
		return err
	}
	if have, err := blob.GetBytes(ctx, uri); err == nil &&
		catalog.VerifyAsset(digest, have) == nil {
		return nil
	}
	return blob.PutBytes(ctx, uri, content)
}

// GetAsset returns the content under a digest, verified against it.
func (s *Store) GetAsset(ctx context.Context, digest string) ([]byte, error) {
	uri, err := s.uriFor(ctx, digest)
	if err != nil {
		return nil, err
	}
	content, err := blob.GetBytes(ctx, uri)
	if err != nil {
		return nil, err
	}
	// The bytes are about to be written into a job's config tree and executed
	// by a build step, so a mismatch has to stop here rather than be reported
	// later by whatever the script does.
	if err := catalog.VerifyAsset(digest, content); err != nil {
		return nil, err
	}
	return content, nil
}
