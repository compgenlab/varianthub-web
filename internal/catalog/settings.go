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
//
// A prefix change also rewrites the snapshot defaults that named the source's
// fields, because those are stored denormalized as plain strings. Without it,
// setting a prefix silently invalidates every snapshot pinning that source: the
// bundle still lists auto_id, materialization now emits CG_auto_id, and every
// job against it fails at annotate time with "unknown annotation" — a long way
// from the settings form that caused it.
func (s *Store) PutSettings(ctx context.Context, sourceID string, set SourceSettings) error {
	before, err := s.effectiveNames(ctx, sourceID)
	if err != nil {
		return err
	}

	if set.Empty() {
		_, err = s.pool.Exec(ctx, `DELETE FROM source_settings WHERE source_id=$1`, sourceID)
	} else {
		_, err = s.pool.Exec(ctx, `
			INSERT INTO source_settings (source_id, annotation_prefix, cache_setup, updated_at)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (source_id) DO UPDATE
			   SET annotation_prefix = excluded.annotation_prefix,
			       cache_setup       = excluded.cache_setup,
			       updated_at        = excluded.updated_at`,
			sourceID, strings.TrimSpace(set.AnnotationPrefix), set.CacheSetup, s.nowFn())
	}
	if err != nil {
		return err
	}

	after, err := s.effectiveNames(ctx, sourceID)
	if err != nil {
		return err
	}
	return s.renameSnapshotDefaults(ctx, sourceID, before, after)
}

// effectiveNames lists a source's output names as they stand right now, indexed
// by the manifest name they came from — the stable identity across a rename.
func (s *Store) effectiveNames(ctx context.Context, sourceID string) (map[string]string, error) {
	src, err := s.GetSource(ctx, sourceID)
	if err != nil {
		return nil, nil // a source that is not there yet has no names to carry
	}
	// Pair manifest name to emitted name positionally: both lists are built from
	// the same manifest in the same order, so index i is the same annotation.
	raw, eff := AnnotationsFromTOML(src.TOML), src.Annotations()
	out := make(map[string]string, len(raw))
	for i := range raw {
		if i < len(eff) {
			out[raw[i].Name] = eff[i].Name
		}
	}
	return out, nil
}

// renameSnapshotDefaults carries a rename into the bundles that pinned the old
// names, leaving every other default untouched.
//
// Only defaults belonging to this source are considered: two sources can emit
// the same bare name, and rewriting another source's default because the string
// matched would silently repoint it.
func (s *Store) renameSnapshotDefaults(ctx context.Context, sourceID string, before, after map[string]string) error {
	rename := map[string]string{}
	for manifestName, old := range before {
		if now, ok := after[manifestName]; ok && now != old {
			rename[old] = now
		}
	}
	if len(rename) == 0 {
		return nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT sn.id, sn.defaults
		  FROM snapshot sn
		  JOIN snapshot_source ss ON ss.snapshot_id = sn.id
		 WHERE ss.source_id = $1`, sourceID)
	if err != nil {
		return err
	}
	type update struct {
		id   string
		defs []string
	}
	var todo []update
	for rows.Next() {
		var u update
		if err := rows.Scan(&u.id, &u.defs); err != nil {
			rows.Close()
			return err
		}
		changed := false
		for i, d := range u.defs {
			if now, ok := rename[d]; ok {
				u.defs[i], changed = now, true
			}
		}
		if changed {
			todo = append(todo, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, u := range todo {
		if _, err := s.pool.Exec(ctx,
			`UPDATE snapshot SET defaults=$2, updated_at=$3 WHERE id=$1`,
			u.id, u.defs, s.nowFn()); err != nil {
			return err
		}
	}
	return nil
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
