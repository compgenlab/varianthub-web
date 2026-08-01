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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// StateAdhoc marks a snapshot generated from an individual-source selection.
	// It is a real snapshot — materialized and reproducible like any other — but
	// never offered in the picker or the admin list.
	StateAdhoc = "adhoc"
)

// Source is one registered annotation source.
type Source struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Kind    string `json:"kind"`
	Build   string `json:"build,omitempty"`

	// Stream is derived from the manifest on read rather than stored, like the
	// annotation list: it is a projection of toml_text, and deriving it means a
	// source registered before this existed needs no backfill.
	Stream bool `json:"stream,omitempty"`

	Visibility  string `json:"visibility"`
	IndexStatus string `json:"index_status"`
	Origin      string `json:"origin,omitempty"`
	TOML        string `json:"-"` // never serialized to API clients by default
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Ref is the "name:version" reference a snapshot manifest uses.
func (s Source) Ref() string { return s.Name + ":" + s.Version }

// NeedsData reports whether the source has files to download.
//
// A builtin annotator computes its values from the variant itself — auto_id,
// tstv, indel need nothing on disk — so it is usable the moment it is
// registered. Offering to provision one is offering to fetch nothing: the job
// runs, downloads zero files, and reports success, which reads as though
// something happened.
func (s Source) NeedsData() bool {
	// A builtin computes from the variant; a streamed source is read from its
	// url. Neither has files, so offering to download them provisions nothing
	// and then reports success.
	return s.Kind != "builtin" && !s.Stream
}

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
	if err == nil {
		s.Stream = streamFromTOML(s.TOML)
	}
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
	// Ad-hoc rows are machine-generated per submission; they are snapshots for the
	// engine's purposes but not things a person picks from a list.
	rows, err := s.pool.Query(ctx,
		`SELECT `+snapshotCols+` FROM snapshot WHERE state <> $1
		 ORDER BY created_at DESC, id`, StateAdhoc)
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
		// Default closed. Publishing something private is a disclosure that
		// cannot be undone; a private source nobody can see is a support
		// request. The costs are not symmetric.
		src.Visibility = VisibilityPrivate
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

// ErrPinsFrozen is returned when a published snapshot's source pins would change.
var ErrPinsFrozen = errors.New("a published snapshot's pinned versions cannot change")

// UpdateSnapshotMeta edits a snapshot's presentation and default annotations
// without touching its pins.
//
// This is allowed on a published snapshot. What makes a snapshot reproducible is
// the set of pinned source *versions*; a title, a description, or which fields
// are checked by default are conveniences, and a job records the annotations it
// actually ran with regardless. Freezing those too would mean a typo in a title
// could never be fixed.
func (s *Store) UpdateSnapshotMeta(ctx context.Context, snap Snapshot) error {
	if snap.Defaults == nil {
		snap.Defaults = []string{}
	}
	if snap.Tags == nil {
		snap.Tags = []string{}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE snapshot
		   SET title=$2, description=$3, defaults=$4, tags=$5, updated_at=$6
		 WHERE id=$1`,
		snap.ID, snap.Title, snap.Description, snap.Defaults, snap.Tags, s.nowFn())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("snapshot %q: %w", snap.ID, ErrNotFound)
	}
	return nil
}

// DeleteSnapshot removes a snapshot and its pins.
//
// Allowed for a published snapshot: publishing fixes what a snapshot *means*, it
// does not make the row permanent. Jobs reference a snapshot by name and store
// their own column model, so existing results stay readable — only new
// annotation against that name stops working.
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM snapshot WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("snapshot %q: %w", id, ErrNotFound)
	}
	return nil
}

// pinsOf returns a snapshot's pinned source ids, sorted, plus whether the
// snapshot exists and its current state.
func (s *Store) pinsOf(ctx context.Context, id string) (ids []string, state string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT state FROM snapshot WHERE id=$1`, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	rows, qErr := s.pool.Query(ctx,
		`SELECT source_id FROM snapshot_source WHERE snapshot_id=$1`, id)
	if qErr != nil {
		return nil, "", false, qErr
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, "", false, err
		}
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	return ids, state, true, rows.Err()
}

// PutSnapshot inserts or updates a snapshot and replaces its source pins. The
// whole thing is one transaction: a snapshot is never briefly visible with a
// partial source list, which would materialize into a manifest missing sources.
//
// Refuses to change the pins of an already-published snapshot — that is the one
// thing publishing fixes. Use UpdateSnapshotMeta for everything else.
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
	// Enforced here rather than in the handlers because this is the single point
	// every path to a snapshot's membership goes through — creating one, editing
	// one, and the ad-hoc snapshot an individual-source selection mints.
	// Checking it in one caller left the other two accepting a mix, and a wrong
	// assembly does not error at annotate time: it returns plausible answers at
	// coordinates that mean something else.
	if err := s.checkBuilds(ctx, snap.Build, sourceIDs); err != nil {
		return err
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
	existing, state, found, err := s.pinsOf(ctx, snap.ID)
	if err != nil {
		return err
	}
	if found && state == StatePublished {
		want := append([]string(nil), sourceIDs...)
		sort.Strings(want)
		if !slices.Equal(existing, want) {
			return fmt.Errorf("%w (snapshot %q)", ErrPinsFrozen, snap.ID)
		}
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
			// A foreign-key violation means the source id does not exist. Say that,
			// rather than passing a raw constraint error up to an API client.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return fmt.Errorf("%w: unknown source %q", ErrNotFound, id)
			}
			return fmt.Errorf("pin source %q to snapshot %q: %w", id, snap.ID, err)
		}
	}
	return tx.Commit(ctx)
}

// AdhocID is the deterministic id for a selection of sources on a build.
//
// Deterministic so repeat submissions of the same selection reuse one row rather
// than accumulating a snapshot per job. Sorted first, because the same set chosen
// in a different order is the same selection.
func AdhocID(build string, sourceIDs []string) string {
	ids := append([]string(nil), sourceIDs...)
	sort.Strings(ids)
	h := sha256.Sum256([]byte(build + "\x00" + strings.Join(ids, "\x00")))
	return "adhoc-" + hex.EncodeToString(h[:6])
}

// EnsureAdhocSnapshot creates (or reuses) a snapshot for an ad-hoc selection and
// returns its id.
func (s *Store) EnsureAdhocSnapshot(ctx context.Context, build string, sourceIDs []string,
	defaults []string) (string, error) {

	if build == "" {
		return "", errors.New("a build is required to annotate with individual sources")
	}
	if len(sourceIDs) == 0 {
		return "", errors.New("select at least one source")
	}
	id := AdhocID(build, sourceIDs)
	if err := s.PutSnapshot(ctx, Snapshot{
		ID:          id,
		Title:       "Ad-hoc selection",
		Description: "Generated from an individual-source selection.",
		Build:       build,
		State:       StateAdhoc,
		Defaults:    defaults,
	}, sourceIDs); err != nil {
		return "", err
	}
	return id, nil
}

// Count returns the number of snapshots and sources. Used by the seed command to
// decide whether there is anything to do.
func (s *Store) Count(ctx context.Context) (snapshots, sources int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM snapshot), (SELECT count(*) FROM source)`).
		Scan(&snapshots, &sources)
	return
}

// SourceSnapshots lists the real snapshots pinning a source.
//
// Ad-hoc rows are excluded: they are generated per submission from an
// individual-source selection, are hidden from every listing, and would
// otherwise make a source permanently undeletable after one ad-hoc annotation —
// blocked by a snapshot the user has no way to find or remove.
func (s *Store) SourceSnapshots(ctx context.Context, sourceID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ss.snapshot_id
		  FROM snapshot_source ss
		  JOIN snapshot sn ON sn.id = ss.snapshot_id
		 WHERE ss.source_id = $1 AND sn.state <> $2
		 ORDER BY ss.snapshot_id`, sourceID, StateAdhoc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ErrSourcePinned is returned when deleting a source a snapshot still pins.
var ErrSourcePinned = errors.New("source is pinned by a snapshot")

// DeleteSource removes a source that no snapshot pins, and returns where its
// files were so the caller can reclaim the disk.
//
// A pinned source is refused rather than cascaded: removing it would silently
// change what those snapshots mean, and a published snapshot is a promise that
// its pinned versions do not move. Detach it first.
//
// The file *records* go with the row (ON DELETE CASCADE); the bytes do not —
// only the worker mounts the storage, so reclaiming them is a job.
func (s *Store) DeleteSource(ctx context.Context, id string) (src Source, locations []StorageLocation, err error) {
	pinned, err := s.SourceSnapshots(ctx, id)
	if err != nil {
		return Source{}, nil, err
	}
	if len(pinned) > 0 {
		return Source{}, nil, fmt.Errorf("%w (%s)", ErrSourcePinned, strings.Join(pinned, ", "))
	}

	// Ad-hoc snapshots pinning it are regenerable — the same selection produces
	// the same id — and past job results are self-contained, carrying their own
	// column model. So they go with the source rather than blocking it.
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM snapshot
		 WHERE state = $1
		   AND id IN (SELECT snapshot_id FROM snapshot_source WHERE source_id = $2)`,
		StateAdhoc, id); err != nil {
		return Source{}, nil, err
	}

	row := s.pool.QueryRow(ctx, `SELECT `+sourceCols+` FROM source WHERE id=$1`, id)
	src, err = scanSource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, nil, fmt.Errorf("source %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Source{}, nil, err
	}

	// Capture the locations before the cascade removes the file rows.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT sl.id, sl.name, sl.kind, sl.uri
		  FROM source_file sf JOIN storage_location sl ON sl.id = sf.storage_id
		 WHERE sf.source_id = $1`, id)
	if err != nil {
		return Source{}, nil, err
	}
	for rows.Next() {
		var l StorageLocation
		if err := rows.Scan(&l.ID, &l.Name, &l.Kind, &l.URI); err != nil {
			rows.Close()
			return Source{}, nil, err
		}
		locations = append(locations, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Source{}, nil, err
	}

	if _, err := s.pool.Exec(ctx, `DELETE FROM source WHERE id=$1`, id); err != nil {
		return Source{}, nil, err
	}
	return src, locations, nil
}

// SetSnapshotSources replaces a draft snapshot's source set.
//
// Published snapshots are refused: a published snapshot is a reproducibility
// claim, and changing which sources it contains would silently change what
// every past result meant. Editing its metadata stays allowed — that is what
// ErrPinsFrozen distinguishes.
//
// The build invariant is enforced here rather than in the handler because this
// is the choke point every path to a snapshot's membership goes through.
func (s *Store) SetSnapshotSources(ctx context.Context, id string, sourceIDs []string) (Snapshot, error) {
	snap, err := s.GetSnapshot(ctx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.State == StatePublished {
		return Snapshot{}, fmt.Errorf("%w (snapshot %q)", ErrPinsFrozen, id)
	}
	if len(sourceIDs) == 0 {
		return Snapshot{}, errors.New("a snapshot needs at least one source")
	}
	// Sources are carried separately; PutSnapshot takes the ids, and enforces
	// the assembly invariant for every path including this one.
	snap.Sources = nil
	if err := s.PutSnapshot(ctx, snap, sourceIDs); err != nil {
		return Snapshot{}, err
	}
	return s.GetSnapshot(ctx, id)
}

// checkBuilds refuses a source whose declared assembly differs from the
// snapshot's.
//
// A wrong assembly does not error at annotate time — it returns plausible wrong
// answers at coordinates that mean something else, which is the one failure
// mode invisible in the output. Sources with no declared build are
// assembly-agnostic (a builtin computes from the variant) and belong anywhere.
func (s *Store) checkBuilds(ctx context.Context, build string, sourceIDs []string) error {
	rows, err := s.pool.Query(ctx,
		`SELECT id, build FROM source WHERE id = ANY($1) AND build <> '' AND build <> $2`,
		sourceIDs, build)
	if err != nil {
		return err
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var id, b string
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		bad = append(bad, fmt.Sprintf("%s is %s", id, b))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("snapshot build is %s but %s — a snapshot cannot mix assemblies",
			build, strings.Join(bad, ", "))
	}
	return nil
}

// GetSource returns one source, manifest included.
func (s *Store) GetSource(ctx context.Context, id string) (Source, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+sourceCols+` FROM source WHERE id=$1`, id)
	src, err := scanSource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, fmt.Errorf("source %q: %w", id, ErrNotFound)
	}
	return src, err
}
