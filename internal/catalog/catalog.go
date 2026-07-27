// Package catalog is the annotation catalog: which sources and snapshots exist,
// stored in Postgres.
//
// Manifests are kept as TOML text and handed to varhub unchanged. The service
// never parses or rewrites them — it stores what an admin wrote, materializes it
// to a directory, and lets the CLI's own parser be the authority. The columns
// beside toml_text are a projection for listing and filtering, derived at write
// time; toml_text is the source of truth.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a snapshot or source id does not exist.
var ErrNotFound = errors.New("not found")

// Visibility values.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Snapshot states.
const (
	StateDraft     = "draft"
	StatePublished = "published"
)

// Source is one registered annotation source.
type Source struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Title       string `json:"title,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Kind        string `json:"kind"`
	Build       string `json:"build,omitempty"`
	Visibility  string `json:"visibility"`
	IndexStatus string `json:"index_status"`
	Origin      string `json:"origin,omitempty"`
	TOML        string `json:"-"` // never serialized to API clients by default
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Ref is the "name:version" reference a snapshot manifest uses.
func (s Source) Ref() string { return s.Name + ":" + s.Version }

// Snapshot is a versioned bundle of pinned sources.
type Snapshot struct {
	ID          string   `json:"id"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Build       string   `json:"build"`
	State       string   `json:"state"`
	Defaults    []string `json:"defaults,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	PublishedAt int64    `json:"published_at,omitempty"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`

	Sources []Source `json:"sources,omitempty"` // populated by Get, not List
}

// ContainsPrivate reports whether any pinned source is private. Drives the lock
// notice on the snapshot cards.
func (s Snapshot) ContainsPrivate() bool {
	for _, src := range s.Sources {
		if src.Visibility == VisibilityPrivate {
			return true
		}
	}
	return false
}

// Store reads and writes the catalog.
type Store struct {
	pool  *pgxpool.Pool
	nowFn func() int64
}

// New wraps a pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, nowFn: func() int64 { return time.Now().Unix() }}
}

// Open connects to Postgres and returns a Store. The caller must Close it.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return New(pool), nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

const sourceCols = `id, name, version, title, detail, kind, build, visibility,
	index_status, origin, toml_text, created_at, updated_at`

func scanSource(row interface{ Scan(...any) error }) (Source, error) {
	var s Source
	err := row.Scan(&s.ID, &s.Name, &s.Version, &s.Title, &s.Detail, &s.Kind,
		&s.Build, &s.Visibility, &s.IndexStatus, &s.Origin, &s.TOML,
		&s.CreatedAt, &s.UpdatedAt)
	return s, err
}

const snapshotCols = `id, title, description, build, state, defaults, tags,
	COALESCE(published_at,0), created_at, updated_at`

func scanSnapshot(row interface{ Scan(...any) error }) (Snapshot, error) {
	var s Snapshot
	err := row.Scan(&s.ID, &s.Title, &s.Description, &s.Build, &s.State,
		&s.Defaults, &s.Tags, &s.PublishedAt, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// ListSources returns every source, newest first.
func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sourceCols+` FROM source ORDER BY name, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// ListSnapshots returns every snapshot without its sources.
func (s *Store) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+snapshotCols+` FROM snapshot ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// GetSnapshot returns one snapshot with its pinned sources, in order.
func (s *Store) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+snapshotCols+` FROM snapshot WHERE id=$1`, id)
	snap, err := scanSnapshot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("snapshot %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Snapshot{}, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+sourceCols+`
		FROM source
		JOIN snapshot_source ss ON ss.source_id = source.id
		WHERE ss.snapshot_id = $1
		ORDER BY ss.position, source.name`, id)
	if err != nil {
		return Snapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return Snapshot{}, err
		}
		snap.Sources = append(snap.Sources, src)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, err
	}
	if len(snap.Sources) == 0 {
		// A snapshot with no sources materializes to a manifest varhub rejects.
		// Catching it here names the real problem.
		return Snapshot{}, fmt.Errorf("snapshot %q pins no sources", id)
	}
	return snap, nil
}

// PutSource inserts or updates a source by id.
func (s *Store) PutSource(ctx context.Context, src Source) error {
	if src.ID == "" || src.Name == "" || src.Version == "" {
		return errors.New("source needs id, name and version")
	}
	if src.TOML == "" {
		return fmt.Errorf("source %q has no TOML manifest", src.ID)
	}
	if src.Visibility == "" {
		src.Visibility = VisibilityPublic
	}
	if src.IndexStatus == "" {
		src.IndexStatus = "indexed"
	}
	now := s.nowFn()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO source (id,name,version,title,detail,kind,build,visibility,
		                    index_status,origin,toml_text,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)
		ON CONFLICT (id) DO UPDATE SET
		  name=excluded.name, version=excluded.version, title=excluded.title,
		  detail=excluded.detail, kind=excluded.kind, build=excluded.build,
		  visibility=excluded.visibility, index_status=excluded.index_status,
		  origin=excluded.origin, toml_text=excluded.toml_text,
		  updated_at=excluded.updated_at`,
		src.ID, src.Name, src.Version, src.Title, src.Detail, src.Kind, src.Build,
		src.Visibility, src.IndexStatus, src.Origin, src.TOML, now)
	return err
}

// PutSnapshot inserts or updates a snapshot and replaces its source pins. The
// whole thing is one transaction: a snapshot is never briefly visible with a
// partial source list, which would materialize into a manifest missing sources.
func (s *Store) PutSnapshot(ctx context.Context, snap Snapshot, sourceIDs []string) error {
	if snap.ID == "" {
		return errors.New("snapshot needs an id")
	}
	if snap.Build == "" {
		return fmt.Errorf("snapshot %q needs a build (assembly)", snap.ID)
	}
	if snap.State == "" {
		snap.State = StateDraft
	}
	// A nil Go slice binds as SQL NULL, and a column DEFAULT does not apply when
	// NULL is passed explicitly — so nil would violate the NOT NULL constraint
	// rather than fall back to '{}'.
	if snap.Defaults == nil {
		snap.Defaults = []string{}
	}
	if snap.Tags == nil {
		snap.Tags = []string{}
	}
	now := s.nowFn()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var published any
	if snap.State == StatePublished {
		if snap.PublishedAt != 0 {
			published = snap.PublishedAt
		} else {
			published = now
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO snapshot (id,title,description,build,state,defaults,tags,
		                      published_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)
		ON CONFLICT (id) DO UPDATE SET
		  title=excluded.title, description=excluded.description,
		  build=excluded.build, state=excluded.state, defaults=excluded.defaults,
		  tags=excluded.tags, published_at=excluded.published_at,
		  updated_at=excluded.updated_at`,
		snap.ID, snap.Title, snap.Description, snap.Build, snap.State,
		snap.Defaults, snap.Tags, published, now); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM snapshot_source WHERE snapshot_id=$1`, snap.ID); err != nil {
		return err
	}
	for i, id := range sourceIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO snapshot_source (snapshot_id,source_id,position) VALUES ($1,$2,$3)`,
			snap.ID, id, i); err != nil {
			return fmt.Errorf("pin source %q to snapshot %q: %w", id, snap.ID, err)
		}
	}
	return tx.Commit(ctx)
}

// Count returns the number of snapshots and sources. Used by the seed command to
// decide whether there is anything to do.
func (s *Store) Count(ctx context.Context) (snapshots, sources int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM snapshot), (SELECT count(*) FROM source)`).
		Scan(&snapshots, &sources)
	return
}
