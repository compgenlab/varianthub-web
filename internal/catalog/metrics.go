package catalog

import (
	"context"
	"strings"
)

// StorageUsage is what one location holds.
type StorageUsage struct {
	StorageID string `json:"storage_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	URI       string `json:"uri"`
	// Bucket is set for an S3 location, so usage can be read per bucket even
	// when several locations share one.
	Bucket    string `json:"bucket,omitempty"`
	Bytes     int64  `json:"bytes"`
	Files     int64  `json:"files"`
	Sources   int64  `json:"sources"`
	IsDefault bool   `json:"is_default"`
}

// SourceCounts breaks the catalog down by how a source gets its data.
type SourceCounts struct {
	Total int64 `json:"total"`
	// Provisioned sources have files recorded in at least one location.
	Provisioned int64 `json:"provisioned"`
	// Streamed sources are read from their origin and stored nowhere.
	Streamed int64 `json:"streamed"`
	// Builtins compute from the variant and have no data at all.
	Builtin int64 `json:"builtin"`
	// Pending is the rest: registered, needing data, not yet downloaded.
	Pending int64 `json:"pending"`
}

// StorageUsage reports bytes held per location.
//
// Every location appears, including empty ones: "this bucket is configured and
// holds nothing" is the answer an operator is usually looking for, and dropping
// the row would make it indistinguishable from a location that does not exist.
func (s *Store) StorageUsage(ctx context.Context) ([]StorageUsage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, l.name, l.kind, l.uri, l.is_default,
		       coalesce(sum(f.size_bytes), 0),
		       count(f.path),
		       count(DISTINCT f.source_id)
		  FROM storage_location l
		  LEFT JOIN source_file f ON f.storage_id = l.id
		 GROUP BY l.id, l.name, l.kind, l.uri, l.is_default
		 ORDER BY l.is_default DESC, l.kind, l.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StorageUsage{}
	for rows.Next() {
		var u StorageUsage
		if err := rows.Scan(&u.StorageID, &u.Name, &u.Kind, &u.URI, &u.IsDefault,
			&u.Bytes, &u.Files, &u.Sources); err != nil {
			return nil, err
		}
		if u.Kind == StorageS3 {
			u.Bucket = bucketOf(u.URI)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// bucketOf pulls the bucket out of an s3://bucket/prefix URI.
func bucketOf(uri string) string {
	rest := strings.TrimPrefix(uri, "s3://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// CountSources classifies the catalog by where each source's data lives.
//
// Streaming is read from toml_text rather than a column, matching how the rest
// of the projection works — a source registered before `stream` existed needs no
// backfill.
func (s *Store) CountSources(ctx context.Context) (SourceCounts, error) {
	var c SourceCounts
	rows, err := s.pool.Query(ctx, `
		SELECT s.kind, s.toml_text,
		       EXISTS (SELECT 1 FROM source_file f WHERE f.source_id = s.id)
		  FROM source s`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, text string
		var hasFiles bool
		if err := rows.Scan(&kind, &text, &hasFiles); err != nil {
			return c, err
		}
		c.Total++
		switch {
		case hasFiles:
			c.Provisioned++
		case kind == "builtin":
			c.Builtin++
		case streamFromTOML(text):
			c.Streamed++
		default:
			c.Pending++
		}
	}
	return c, rows.Err()
}
