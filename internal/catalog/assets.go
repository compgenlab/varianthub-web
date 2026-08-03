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
		if _, err := tx.Exec(ctx,
			`INSERT INTO source_asset (source_id,name,content,created_at) VALUES ($1,$2,$3,$4)`,
			sourceID, a.Name, []byte(a.Content), now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Assets returns a source's stored helper files.
func (s *Store) Assets(ctx context.Context, sourceID string) ([]Asset, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, content FROM source_asset WHERE source_id=$1 ORDER BY name`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		var a Asset
		var content []byte
		if err := rows.Scan(&a.Name, &content); err != nil {
			return nil, err
		}
		a.Content = string(content)
		out = append(out, a)
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
		`SELECT source_id, name, content FROM source_asset
		  WHERE source_id = ANY($1) ORDER BY source_id, name`, sourceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var a Asset
		var content []byte
		if err := rows.Scan(&id, &a.Name, &content); err != nil {
			return nil, err
		}
		a.Content = string(content)
		out[id] = append(out[id], a)
	}
	return out, rows.Err()
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
