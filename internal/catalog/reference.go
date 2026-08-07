package catalog

import (
	"context"
	"fmt"
	"strings"
)

// Reference is a reference genome the installation has, or is fetching.
//
// Catalog data rather than configuration. It used to be a config map of
// assembly to path, which meant adding one required editing a file on the host
// and restarting, and only ever pointed at a path that already existed there.
// Which references exist is a fact about the installation, like its sources.
type Reference struct {
	Assembly string `json:"assembly"`
	// URI is where the bytes come from: https://, s3://, or a file staged in the
	// admin scratch area. Kept after provisioning, so the same reference can be
	// re-fetched onto another machine and so it is possible to see what a file
	// actually is.
	URI      string `json:"uri"`
	Checksum string `json:"checksum,omitempty"`
	// Path is where it landed on the worker's filesystem; empty until
	// provisioned. A path and not a locator, because a tool step binds the
	// FASTA's directory into a container — this is the one thing that cannot
	// live in an object store however much the source data does.
	Path      string `json:"path,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// StorageID names where the durable copy is kept, and DurableURI is where it
	// landed there. The local copy under data_dir is what a tool actually reads;
	// this is what lets another worker restore rather than re-fetch.
	StorageID  string `json:"storage_id,omitempty"`
	DurableURI string `json:"durable_uri,omitempty"`

	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Reference states, matching source_state so the UI renders them the same way.
const (
	RefInstalling = "installing"
	RefReady      = "ready"
	RefFailed     = "failed"
)

const refCols = `assembly, uri, checksum, path, size_bytes, storage_id, durable_uri, state, error, created_at, updated_at`

func scanRef(row interface{ Scan(...any) error }) (Reference, error) {
	var r Reference
	err := row.Scan(&r.Assembly, &r.URI, &r.Checksum, &r.Path, &r.SizeBytes,
		&r.StorageID, &r.DurableURI, &r.State, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// PutReference records a reference, replacing any for the same assembly.
//
// One per assembly on purpose: a tool step rendering {ref} asks "the FASTA for
// GRCh38", and a second answer would make that question ambiguous rather than
// richer. Replacing resets the state, because new bytes are not provisioned
// until they have been fetched.
func (s *Store) PutReference(ctx context.Context, r Reference) error {
	assembly := strings.TrimSpace(r.Assembly)
	if assembly == "" {
		return fmt.Errorf("reference: assembly is required")
	}
	if strings.TrimSpace(r.URI) == "" {
		return fmt.Errorf("reference %s: a source URI is required", assembly)
	}
	now := s.nowFn()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO reference (assembly, uri, checksum, path, size_bytes, storage_id,
		                       durable_uri, state, error, created_at, updated_at)
		VALUES ($1,$2,$3,'',0,$4,'',$5,'',$6,$6)
		ON CONFLICT (assembly) DO UPDATE
		   SET uri         = excluded.uri,
		       checksum    = excluded.checksum,
		       path        = '',
		       size_bytes  = 0,
		       storage_id  = excluded.storage_id,
		       durable_uri = '',
		       state       = excluded.state,
		       error       = '',
		       updated_at  = excluded.updated_at`,
		assembly, strings.TrimSpace(r.URI), strings.TrimSpace(r.Checksum),
		strings.TrimSpace(r.StorageID), RefInstalling, now)
	return err
}

// SetReferenceReady records where a fetched reference landed.
func (s *Store) SetReferenceReady(ctx context.Context, assembly, path string, size int64,
	durableURI string) error {

	_, err := s.pool.Exec(ctx, `
		UPDATE reference
		   SET path=$2, size_bytes=$3, durable_uri=$4, state=$5, error='', updated_at=$6
		 WHERE assembly=$1`,
		assembly, path, size, durableURI, RefReady, s.nowFn())
	return err
}

// SetReferenceFailed records why a fetch did not finish.
func (s *Store) SetReferenceFailed(ctx context.Context, assembly, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE reference SET state=$2, error=$3, updated_at=$4 WHERE assembly=$1`,
		assembly, RefFailed, reason, s.nowFn())
	return err
}

// Reference returns one assembly's reference.
func (s *Store) Reference(ctx context.Context, assembly string) (Reference, bool, error) {
	r, err := scanRef(s.pool.QueryRow(ctx,
		`SELECT `+refCols+` FROM reference WHERE assembly=$1`, assembly))
	if err != nil {
		return Reference{}, false, nil
	}
	return r, true, nil
}

// ListReferences returns every reference, by assembly.
func (s *Store) ListReferences(ctx context.Context) ([]Reference, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+refCols+` FROM reference ORDER BY assembly`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reference{}
	for rows.Next() {
		r, err := scanRef(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteReference forgets a reference. The file itself is left alone: it may be
// shared, and reclaiming disk is a separate decision from forgetting a record.
func (s *Store) DeleteReference(ctx context.Context, assembly string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM reference WHERE assembly=$1`, assembly)
	return err
}

// ReadyReferences maps assembly to filesystem path for every provisioned
// reference — the form the materializer writes into a job's config.
//
// Only ready ones: a half-fetched FASTA named in a job's config is worse than an
// absent one, because a tool renders the path and fails somewhere deep inside
// itself rather than at materialization.
func (s *Store) ReadyReferences(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT assembly, path FROM reference WHERE state=$1 AND path <> ''`, RefReady)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, p string
		if err := rows.Scan(&a, &p); err != nil {
			return nil, err
		}
		out[a] = p
	}
	return out, rows.Err()
}
