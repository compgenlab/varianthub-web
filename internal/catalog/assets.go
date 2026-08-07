package catalog

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/BurntSushi/toml"
)

// MaxAssetBytes caps one helper file. Recipe assets are scripts — a few KB —
// so this is generous enough not to bind in practice while keeping a manifest
// from pulling something the database has no business holding.
const MaxAssetBytes = 1 << 20

// Asset is a helper file a source's recipe needs, stored with the source.
type Asset struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// assetFragment is the asset half of a source manifest.
type assetFragment struct {
	Sources []struct {
		// Tool sources list step assets at the top level.
		Assets []string `toml:"assets"`
		Build  *struct {
			Assets []string `toml:"assets"`
		} `toml:"build"`
	} `toml:"sources"`
}

// AssetNames lists the co-located helper files a manifest declares.
//
// URLs and absolute paths are excluded: varhub fetches a URL asset itself at
// run time, and an absolute path refers to the machine the recipe runs on.
// Only relative names ship beside the manifest, so only they need carrying.
func AssetNames(text string) []string {
	var f assetFragment
	if _, err := toml.Decode(text, &f); err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range f.Sources {
		names := append([]string{}, s.Assets...)
		if s.Build != nil {
			names = append(names, s.Build.Assets...)
		}
		for _, n := range names {
			if n == "" || seen[n] || isRemoteAsset(n) {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func isRemoteAsset(n string) bool {
	return strings.HasPrefix(n, "http://") ||
		strings.HasPrefix(n, "https://") ||
		strings.HasPrefix(n, "/")
}

// ValidateAssetName rejects a name that would write outside the source's own
// directory.
//
// The name comes from a manifest, and the manifest may come from a registry
// nobody here controls. "../../etc/something" is the shape to refuse, and it
// costs one check to make the materializer's job unconditionally safe.
func ValidateAssetName(name string) error {
	if name == "" {
		return fmt.Errorf("asset name is empty")
	}
	if isRemoteAsset(name) {
		return fmt.Errorf("asset %q is a URL or absolute path; those are not stored", name)
	}
	clean := path.Clean(name)
	if clean != name || strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
		return fmt.Errorf("asset %q must be a plain relative path", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return fmt.Errorf("asset %q must not escape its source directory", name)
		}
	}
	return nil
}

// PutAssets replaces a source's stored helper files.
//
// Replace rather than merge: the manifest decides which assets exist, so one
// left behind by an earlier version would be a file the recipe no longer
// mentions, sitting where a later one might shadow it.
func (s *Store) PutAssets(ctx context.Context, sourceID string, assets []Asset) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM source_asset WHERE source_id=$1`, sourceID); err != nil {
		return err
	}
	now := s.nowFn()
	for _, a := range assets {
		if err := ValidateAssetName(a.Name); err != nil {
			return err
		}
		if len(a.Content) > MaxAssetBytes {
			return fmt.Errorf("asset %q is %d bytes, over the %d byte limit",
				a.Name, len(a.Content), MaxAssetBytes)
		}
		content := []byte(a.Content)
		digest := AssetDigest(content)

		// Upload before the row commits. The other order would let a row claim
		// a digest that is not there, and a job would then fail at the first
		// build step with nothing to say why. An upload with no row is only an
		// unreferenced object.
		if s.blobs != nil {
			if err := s.blobs.PutAsset(ctx, digest, content); err != nil {
				return fmt.Errorf("store asset %q: %w", a.Name, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO source_asset (source_id,name,content,sha256,size_bytes,created_at)
				 VALUES ($1,$2,NULL,$3,$4,$5)`,
				sourceID, a.Name, digest, len(content), now); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO source_asset (source_id,name,content,sha256,size_bytes,created_at)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			sourceID, a.Name, content, digest, len(content), now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Assets returns a source's stored helper files.
func (s *Store) Assets(ctx context.Context, sourceID string) ([]Asset, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, content, sha256 FROM source_asset WHERE source_id=$1 ORDER BY name`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	type pending struct {
		idx    int
		digest string
	}
	var fetch []pending
	for rows.Next() {
		var a Asset
		var content []byte
		var digest string
		if err := rows.Scan(&a.Name, &content, &digest); err != nil {
			return nil, err
		}
		if content == nil {
			fetch = append(fetch, pending{len(out), digest})
		} else {
			a.Content = string(content)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Read the rows out before touching storage: holding a pooled connection
	// open across a network fetch would tie up a connection per asset.
	for _, p := range fetch {
		content, err := s.fetchAsset(ctx, out[p.idx].Name, p.digest)
		if err != nil {
			return nil, err
		}
		out[p.idx].Content = string(content)
	}
	return out, rows.Err()
}

// AssetsFor returns the stored helper files for several sources at once, so
// materializing a snapshot is one query rather than one per source.
func (s *Store) AssetsFor(ctx context.Context, sourceIDs []string) (map[string][]Asset, error) {
	out := map[string][]Asset{}
	if len(sourceIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT source_id, name, content, sha256 FROM source_asset
		  WHERE source_id = ANY($1) ORDER BY source_id, name`, sourceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pending struct {
		id     string
		idx    int
		digest string
	}
	var fetch []pending
	for rows.Next() {
		var id string
		var a Asset
		var content []byte
		var digest string
		if err := rows.Scan(&id, &a.Name, &content, &digest); err != nil {
			return nil, err
		}
		if content == nil {
			fetch = append(fetch, pending{id, len(out[id]), digest})
		} else {
			a.Content = string(content)
		}
		out[id] = append(out[id], a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// After the rows are drained, for the same reason as Assets: a connection
	// per asset held across a network fetch would exhaust the pool on a
	// snapshot with several scripted sources.
	for _, p := range fetch {
		a := &out[p.id][p.idx]
		content, err := s.fetchAsset(ctx, a.Name, p.digest)
		if err != nil {
			return nil, err
		}
		a.Content = string(content)
	}
	return out, nil
}

// MissingAssets names the helper files a manifest declares but the catalog does
// not hold, so a source can say it is incomplete before a job discovers it.
func MissingAssets(text string, have []Asset) []string {
	stored := map[string]bool{}
	for _, a := range have {
		stored[a.Name] = true
	}
	var missing []string
	for _, n := range AssetNames(text) {
		if !stored[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

// fetchAsset resolves one out-of-line asset, verifying it against its digest.
func (s *Store) fetchAsset(ctx context.Context, name, digest string) ([]byte, error) {
	if digest == "" {
		// Neither inline nor addressed. A row is written with one or the other
		// in the same transaction, so this means something wrote it by hand.
		return nil, fmt.Errorf("asset %q has neither content nor a digest", name)
	}
	if s.blobs == nil {
		return nil, fmt.Errorf("asset %q is stored as %s but no asset storage is "+
			"configured for this process", name, digest[:12])
	}
	content, err := s.blobs.GetAsset(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("fetch asset %q (%s): %w", name, digest[:12], err)
	}
	return content, nil
}
