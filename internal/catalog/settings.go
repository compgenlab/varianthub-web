package catalog

import (
	"context"
	"strings"
)

// SourceSettings are the deployment's own decisions about a source, as opposed
// to what the source's manifest says about itself.
//
// The two are stored apart because a manifest can be re-fetched from a registry
// and replaced wholesale; a setting made here has to survive that.
type SourceSettings struct {
	// AnnotationPrefix renames the source's output fields. "-" means none.
	AnnotationPrefix string `json:"annotation_prefix,omitempty"`
	// CacheSetup publishes a tool's setup output to the object store.
	CacheSetup bool `json:"cache_setup,omitempty"`
}

// Empty reports whether nothing has been set, so a row need not be written.
func (s SourceSettings) Empty() bool {
	return strings.TrimSpace(s.AnnotationPrefix) == "" && !s.CacheSetup
}

// PutSettings records a source's settings, removing the row when nothing is set
// so "no settings" is one state rather than two.
func (s *Store) PutSettings(ctx context.Context, sourceID string, set SourceSettings) error {
	if set.Empty() {
		_, err := s.pool.Exec(ctx, `DELETE FROM source_settings WHERE source_id=$1`, sourceID)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO source_settings (source_id, annotation_prefix, cache_setup, updated_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (source_id) DO UPDATE
		   SET annotation_prefix = excluded.annotation_prefix,
		       cache_setup       = excluded.cache_setup,
		       updated_at        = excluded.updated_at`,
		sourceID, strings.TrimSpace(set.AnnotationPrefix), set.CacheSetup, s.nowFn())
	return err
}

// Settings returns one source's settings; the zero value when it has none.
func (s *Store) Settings(ctx context.Context, sourceID string) (SourceSettings, error) {
	var out SourceSettings
	err := s.pool.QueryRow(ctx,
		`SELECT annotation_prefix, cache_setup FROM source_settings WHERE source_id=$1`,
		sourceID).Scan(&out.AnnotationPrefix, &out.CacheSetup)
	if err != nil {
		return SourceSettings{}, nil // absent is the zero value, not an error
	}
	return out, nil
}

// SettingsFor returns settings for several sources at once, so materializing a
// snapshot is one query rather than one per source.
func (s *Store) SettingsFor(ctx context.Context, sourceIDs []string) (map[string]SourceSettings, error) {
	out := map[string]SourceSettings{}
	if len(sourceIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT source_id, annotation_prefix, cache_setup
		  FROM source_settings WHERE source_id = ANY($1)`, sourceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var set SourceSettings
		if err := rows.Scan(&id, &set.AnnotationPrefix, &set.CacheSetup); err != nil {
			return nil, err
		}
		out[id] = set
	}
	return out, rows.Err()
}
