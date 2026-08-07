package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// AssetBlobs stores helper-file content outside the database, addressed by the
// SHA-256 of the content itself.
//
// An interface rather than a concrete store because the catalog has no business
// knowing about buckets: it holds the index, and something that does know how to
// reach storage supplies the bytes. Nil is a supported value — an installation
// with no storage location configured keeps its assets inline, as before.
type AssetBlobs interface {
	// PutAsset stores content under its digest. Storing the same content twice
	// is a no-op, not an error: the name is derived from the bytes, so a second
	// write cannot disagree with the first.
	PutAsset(ctx context.Context, digest string, content []byte) error
	// GetAsset returns the content stored under a digest, verifying it.
	GetAsset(ctx context.Context, digest string) ([]byte, error)
}

// WithAssetBlobs returns a Store that keeps asset content in blobs.
func (s *Store) WithAssetBlobs(b AssetBlobs) *Store {
	c := *s
	c.blobs = b
	return &c
}

// AssetDigest is the content address of an asset.
func AssetDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// VerifyAsset checks content against the digest it was fetched under.
//
// The whole reason for naming an object by its content is that the name is a
// claim the content can settle, so fetching without checking would give up what
// content addressing is for. A mismatch means the object was replaced or
// corrupted, and either way the bytes must not reach a build step that will
// execute them.
func VerifyAsset(digest string, content []byte) error {
	if got := AssetDigest(content); got != digest {
		return fmt.Errorf("asset %s does not match its content (got %s): "+
			"the stored object was replaced or corrupted", digest, got)
	}
	return nil
}

// AssetRow is one stored helper file, as the backfill sees it.
type AssetRow struct {
	SourceID string
	Name     string
	Inline   bool
	Digest   string
	Bytes    int
}

// InlineAssets lists the assets whose bytes are still in the database.
func (s *Store) InlineAssets(ctx context.Context) ([]AssetRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source_id, name, octet_length(content), sha256
		  FROM source_asset WHERE content IS NOT NULL
		 ORDER BY source_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetRow{}
	for rows.Next() {
		var r AssetRow
		if err := rows.Scan(&r.SourceID, &r.Name, &r.Bytes, &r.Digest); err != nil {
			return nil, err
		}
		r.Inline = true
		out = append(out, r)
	}
	return out, rows.Err()
}

// BackfillAssets uploads inline asset content and drops it from the database.
//
// One row at a time, and the content is only cleared after the upload has been
// read back and verified. These bytes are the only copy: a source's registry is
// a remote that may have moved on, so an upload that silently failed and a
// DELETE that trusted it would destroy a recipe with no way to recover it.
//
// Safe to re-run. A row already uploaded has no inline content and is not
// selected; a row whose upload succeeded but whose UPDATE did not is uploaded
// again to the same digest, which is a no-op.
func (s *Store) BackfillAssets(ctx context.Context) (moved int, err error) {
	if s.blobs == nil {
		return 0, fmt.Errorf("no asset storage is configured; " +
			"add a storage location before moving assets out of the database")
	}
	rows, err := s.InlineAssets(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		var content []byte
		if err := s.pool.QueryRow(ctx,
			`SELECT content FROM source_asset WHERE source_id=$1 AND name=$2`,
			r.SourceID, r.Name).Scan(&content); err != nil {
			return moved, fmt.Errorf("read %s/%s: %w", r.SourceID, r.Name, err)
		}
		digest := AssetDigest(content)
		if err := s.blobs.PutAsset(ctx, digest, content); err != nil {
			return moved, fmt.Errorf("upload %s/%s: %w", r.SourceID, r.Name, err)
		}
		// Read it back before letting go of the only other copy. PutAsset skips
		// an upload when the object already exists, so without this a row could
		// be cleared on the strength of an object nobody has ever read.
		got, err := s.blobs.GetAsset(ctx, digest)
		if err != nil {
			return moved, fmt.Errorf("verify %s/%s: %w", r.SourceID, r.Name, err)
		}
		if !bytes.Equal(got, content) {
			return moved, fmt.Errorf("verify %s/%s: stored object differs from the row",
				r.SourceID, r.Name)
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE source_asset SET content=NULL, sha256=$3, size_bytes=$4
			  WHERE source_id=$1 AND name=$2`,
			r.SourceID, r.Name, digest, len(content)); err != nil {
			return moved, fmt.Errorf("clear %s/%s: %w", r.SourceID, r.Name, err)
		}
		moved++
	}
	return moved, nil
}
