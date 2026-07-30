package catalog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Storage kinds.
const (
	StoragePath = "path"
	StorageS3   = "s3"
)

// StorageLocation is somewhere source data can be downloaded to.
type StorageLocation struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	URI        string `json:"uri"`
	FromConfig bool   `json:"from_config"`
	IsDefault  bool   `json:"is_default"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// Usable reports whether downloads can currently target this location.
//
// S3 locations can be configured, but `varhub download` cannot yet write to a
// bucket — that is the CLI-side work tracked as chunk 4a. Saying so here means
// the UI can show an S3 target and explain why it is not selectable, rather than
// offering it and failing at job time.
func (l StorageLocation) Usable() bool { return l.Kind == StoragePath }

// UnusableReason explains Usable() == false.
func (l StorageLocation) UnusableReason() string {
	if l.Kind == StorageS3 {
		return "varhub cannot download to S3 yet; use a filesystem location for now"
	}
	return ""
}

// SourceFile is one downloaded file.
type SourceFile struct {
	SourceID   string `json:"source_id"`
	StorageID  string `json:"storage_id"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	ModifiedAt int64  `json:"modified_at"`
	RecordedAt int64  `json:"recorded_at"`
}

// ValidateStorage checks a location's shape.
func ValidateStorage(l StorageLocation) error {
	if l.ID == "" || l.Name == "" {
		return errors.New("storage needs an id and a name")
	}
	uri := strings.TrimSpace(l.URI)
	switch l.Kind {
	case StoragePath:
		if !filepath.IsAbs(uri) {
			// A relative path would resolve against whatever the worker's working
			// directory happens to be, which is not something an operator can reason
			// about.
			return fmt.Errorf("a filesystem location must be an absolute path, got %q", uri)
		}
	case StorageS3:
		if !strings.HasPrefix(uri, "s3://") {
			return fmt.Errorf("an S3 location must be an s3:// URI, got %q", uri)
		}
	default:
		return fmt.Errorf("storage kind must be %q or %q, got %q", StoragePath, StorageS3, l.Kind)
	}
	return nil
}

// ListStorage returns configured locations, deployment-managed ones first.
func (s *Store) ListStorage(ctx context.Context) ([]StorageLocation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,name,kind,uri,from_config,is_default,created_at,updated_at
		  FROM storage_location ORDER BY is_default DESC, from_config DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StorageLocation{}
	for rows.Next() {
		var l StorageLocation
		if err := rows.Scan(&l.ID, &l.Name, &l.Kind, &l.URI, &l.FromConfig,
			&l.IsDefault, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetStorage returns one location.
func (s *Store) GetStorage(ctx context.Context, id string) (StorageLocation, error) {
	var l StorageLocation
	err := s.pool.QueryRow(ctx, `
		SELECT id,name,kind,uri,from_config,is_default,created_at,updated_at
		  FROM storage_location WHERE id=$1`, id).
		Scan(&l.ID, &l.Name, &l.Kind, &l.URI, &l.FromConfig, &l.IsDefault,
			&l.CreatedAt, &l.UpdatedAt)
	if err != nil {
		return StorageLocation{}, fmt.Errorf("storage %q: %w", id, ErrNotFound)
	}
	return l, nil
}

// DefaultStorage returns the location downloads use when none is named.
func (s *Store) DefaultStorage(ctx context.Context) (StorageLocation, error) {
	locs, err := s.ListStorage(ctx)
	if err != nil {
		return StorageLocation{}, err
	}
	for _, l := range locs {
		if l.IsDefault && l.Usable() {
			return l, nil
		}
	}
	for _, l := range locs {
		if l.Usable() {
			return l, nil
		}
	}
	return StorageLocation{}, errors.New("no usable storage location is configured")
}

// PutStorage adds or updates a location.
func (s *Store) PutStorage(ctx context.Context, l StorageLocation) error {
	if err := ValidateStorage(l); err != nil {
		return err
	}
	now := s.nowFn()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO storage_location (id,name,kind,uri,from_config,is_default,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
		ON CONFLICT (id) DO UPDATE SET
		  name=excluded.name, kind=excluded.kind, uri=excluded.uri,
		  from_config=excluded.from_config, is_default=excluded.is_default,
		  updated_at=excluded.updated_at`,
		l.ID, l.Name, l.Kind, strings.TrimSpace(l.URI), l.FromConfig, l.IsDefault, now)
	return err
}

// DeleteStorage removes a location. Deployment-managed locations are refused:
// they come from the service config and would reappear on the next start, so
// removing one would only look like it worked.
func (s *Store) DeleteStorage(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM storage_location WHERE id=$1 AND NOT from_config`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage %q: %w (or it is managed by the deployment config)", id, ErrNotFound)
	}
	return nil
}

// SyncConfigStorage reconciles the deployment-declared locations.
//
// Called at startup so the config file stays authoritative for the locations it
// declares: one removed from the config is removed here, so a stale row cannot
// keep offering a path the deployment no longer mounts.
func (s *Store) SyncConfigStorage(ctx context.Context, locs []StorageLocation) error {
	keep := make([]string, 0, len(locs))
	for _, l := range locs {
		l.FromConfig = true
		if err := s.PutStorage(ctx, l); err != nil {
			return err
		}
		keep = append(keep, l.ID)
	}
	// Drop config-managed rows that are no longer declared. User-added rows are
	// untouched.
	_, err := s.pool.Exec(ctx,
		`DELETE FROM storage_location WHERE from_config AND NOT (id = ANY($1))`, keep)
	return err
}

// --- downloaded files ---

// ReplaceSourceFiles records the files a source occupies in one location,
// replacing any previous record for that pair.
//
// Replace rather than merge: the record describes what a download produced, and
// a re-download that yields fewer files (a source dropping a per-chromosome
// split, say) must not leave the old ones listed as present.
func (s *Store) ReplaceSourceFiles(ctx context.Context, sourceID, storageID string,
	files []SourceFile) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(ctx,
		`DELETE FROM source_file WHERE source_id=$1 AND storage_id=$2`,
		sourceID, storageID); err != nil {
		return err
	}
	now := s.nowFn()
	for _, f := range files {
		if _, err := tx.Exec(ctx, `
			INSERT INTO source_file (source_id,storage_id,path,size_bytes,modified_at,recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			sourceID, storageID, f.Path, f.SizeBytes, f.ModifiedAt, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SourceFiles lists recorded files, optionally narrowed to one source.
func (s *Store) SourceFiles(ctx context.Context, sourceID string) ([]SourceFile, error) {
	q := `SELECT source_id,storage_id,path,size_bytes,modified_at,recorded_at
	        FROM source_file`
	args := []any{}
	if sourceID != "" {
		q += ` WHERE source_id=$1`
		args = append(args, sourceID)
	}
	q += ` ORDER BY source_id, path`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceFile{}
	for rows.Next() {
		var f SourceFile
		if err := rows.Scan(&f.SourceID, &f.StorageID, &f.Path, &f.SizeBytes,
			&f.ModifiedAt, &f.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
