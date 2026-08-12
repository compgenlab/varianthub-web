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

	"github.com/compgenlab/varianthub-web/internal/anncache"
)

// ErrNotFound is returned when a snapshot or source id does not exist.
var ErrNotFound = errors.New("not found")

// Visibility levels, from least to most restrictive.
//
// Ordered, not just distinct: a snapshot's effective level is the most
// restrictive of its own and every source it pins, which only means anything if
// they compare. VisibilityRank is that order; nothing should compare the strings.
const (
	// VisibilityPublic is anyone who can reach the server, anonymous included.
	VisibilityPublic = "public"
	// VisibilitySignedIn is any account, with no grant needed.
	//
	// The level that was missing. "Not for anonymous visitors" is a property of
	// the deployment rather than of each dataset, and expressing it through team
	// grants meant administration that grew with the catalog to say one thing.
	VisibilitySignedIn = "signed_in"
	// VisibilityRestricted is membership of a team the source is granted to.
	// What used to be called "private"; the meaning is unchanged.
	VisibilityRestricted = "restricted"
)

// VisibilityRank orders the levels so they can be compared. Higher is more
// restrictive. An unrecognized value ranks as the most restrictive there is:
// a level this code does not understand is not one it should hand data out on.
func VisibilityRank(v string) int {
	switch v {
	case VisibilityPublic:
		return 0
	case VisibilitySignedIn:
		return 1
	case VisibilityRestricted:
		return 2
	default:
		return 3
	}
}

// ValidVisibility reports whether v is a level this service knows.
func ValidVisibility(v string) bool {
	return v == VisibilityPublic || v == VisibilitySignedIn || v == VisibilityRestricted
}

// MostRestrictive returns whichever of two levels gives away less.
func MostRestrictive(a, b string) string {
	if VisibilityRank(b) > VisibilityRank(a) {
		return b
	}
	return a
}

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
	ID      string `json:"id" doc:"Stable identifier, unique across the catalog."`
	Name    string `json:"name" doc:"The manifest's name for the dataset, e.g. \"gencode\"."`
	Version string `json:"version" doc:"The dataset's own version, e.g. \"48\". Pinned by snapshots so a run is reproducible."`
	Title   string `json:"title,omitempty" doc:"A human-readable title, where the manifest gives one."`
	Detail  string `json:"detail,omitempty" doc:"A one-line description, where the manifest gives one."`
	Kind    string `json:"kind" doc:"builtin | vcf | bed | gtf | tab | genelist | tool | reference."`
	Build   string `json:"build,omitempty" doc:"The genome assembly this source is for, matched exactly and never normalized: GRCh38 and hg38 are different builds here. Empty means assembly-agnostic, which is what the builtins are."`

	Stream bool `json:"stream,omitempty" doc:"Read from its origin over the network at query time rather than from storage this deployment controls, so a run depends on somebody else's server."`

	Visibility  string `json:"visibility" doc:"public | private. A private source is absent entirely for a caller with no grant, rather than listed and refused."`
	IndexStatus string `json:"index_status" doc:"Whether the source's data has been indexed."`
	Origin      string `json:"origin,omitempty" doc:"The registry this manifest was adopted from, where it was."`

	IsDefaultReference bool `json:"is_default_reference,omitempty" doc:"The reference an ad-hoc selection pins for this assembly. Meaningless on anything that is not a reference source."`

	TOML      string `json:"-"`
	CreatedAt int64  `json:"created_at" doc:"Unix seconds."`
	UpdatedAt int64  `json:"updated_at" doc:"Unix seconds."`

	// prefixOverride is this deployment's annotation prefix for the source,
	// loaded with it by sourceCols. Unexported because it is not part of the
	// API shape: it has already been folded into the names Annotations()
	// reports, and offering it separately invites a second interpretation.
	prefixOverride string
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
	ID          string   `json:"id" doc:"Stable identifier, used to annotate against this snapshot."`
	Title       string   `json:"title,omitempty" doc:"A human-readable title."`
	Description string   `json:"description,omitempty" doc:"What this bundle is for."`
	Build       string   `json:"build" doc:"The genome assembly. Every pinned source either declares this exact string or declares none."`
	State       string   `json:"state" doc:"draft | published | adhoc. Drafts are not offered for annotation."`
	Visibility  string   `json:"visibility" doc:"public | signed_in | restricted. The snapshot's own level; what it is actually offered at is the most restrictive of this and every source it pins, since it cannot promise access to a source the caller may not use."`
	Defaults    []string `json:"defaults,omitempty" doc:"Annotation fields selected when the caller asks for none."`
	Tags        []string `json:"tags,omitempty" doc:"Free-form labels for grouping snapshots."`
	PublishedAt int64    `json:"published_at,omitempty" doc:"Unix seconds."`
	CreatedAt   int64    `json:"created_at" doc:"Unix seconds."`
	UpdatedAt   int64    `json:"updated_at" doc:"Unix seconds."`

	Sources []Source `json:"sources,omitempty" doc:"The exact source versions pinned. Populated when fetching one snapshot, absent from listings."`
}

// ContainsRemote reports whether any pinned source is read from its origin
// rather than from storage this deployment controls.
//
// Worth surfacing next to a snapshot rather than only per source: it is the
// difference between a run that depends on local disk and one that depends on
// somebody else's server being reachable, and that is a property of the whole
// snapshot.
func (s Snapshot) ContainsRemote() bool {
	for _, src := range s.Sources {
		if src.Stream {
			return true
		}
	}
	return false
}

// ContainsPrivate reports whether any pinned source is above public. Drives the
// lock notice on the snapshot cards.
//
// Any restriction counts, not only the strongest: the notice answers "is this
// bundle offered to everyone", and signed_in is as much a no as restricted.
func (s Snapshot) ContainsPrivate() bool {
	for _, src := range s.Sources {
		if src.Visibility != VisibilityPublic {
			return true
		}
	}
	return false
}

// EffectiveVisibility is the level a snapshot is actually offered at: the most
// restrictive of its own and every source it pins.
//
// A snapshot can be narrowed beyond its sources — a bundle assembled for one
// group out of individually public sources — but never widened past them.
// Widening would be a promise the catalog cannot keep: the caller would be handed
// a snapshot whose annotations they are not allowed to compute.
//
// Only meaningful with Sources populated, which is why it is a method on the
// snapshot rather than a stored column. Listings that carry no sources fall back
// to the stored level, which is the floor.
func (s Snapshot) EffectiveVisibility() string {
	out := s.Visibility
	if out == "" {
		out = VisibilityPublic
	}
	for _, src := range s.Sources {
		out = MostRestrictive(out, src.Visibility)
	}
	return out
}

// Store reads and writes the catalog.
type Store struct {
	pool  *pgxpool.Pool
	nowFn func() int64
	// Where asset content lives when it is not inline. Nil keeps it in the
	// database, which is what an installation with no storage configured does.
	blobs AssetBlobs
	// Deployment settings, cached briefly: see site.go. A pointer so a derived
	// store (WithAssetBlobs) shares one cache rather than copying a mutex — and
	// so both see a change the moment either writes one.
	site *siteCache
}

// New wraps a pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:  pool,
		nowFn: func() int64 { return time.Now().Unix() },
		site:  &siteCache{},
	}
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

// Pool exposes the connection pool so the identity store can share it. These
// are the same database; a second pool would double the connection budget for
// no benefit, and the two stores would then hold inconsistent views under load.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// The annotation prefix is joined in rather than left to callers to fetch: it
// renames every field the source contributes, so a Source loaded without it
// reports names that materialization would not produce, and a snapshot built
// from those names fails at annotate time with "unknown annotation" — far from
// the listing that invented them.
const sourceCols = `id, name, version, title, detail, kind, build, visibility,
	index_status, origin, toml_text, created_at, updated_at, is_default_reference,
	COALESCE((SELECT annotation_prefix FROM source_settings st
	          WHERE st.source_id = source.id), '')`

func scanSource(row interface{ Scan(...any) error }) (Source, error) {
	var s Source
	err := row.Scan(&s.ID, &s.Name, &s.Version, &s.Title, &s.Detail, &s.Kind,
		&s.Build, &s.Visibility, &s.IndexStatus, &s.Origin, &s.TOML,
		&s.CreatedAt, &s.UpdatedAt, &s.IsDefaultReference, &s.prefixOverride)
	if err == nil {
		s.Stream = streamFromTOML(s.TOML)
	}
	return s, err
}

const snapshotCols = `id, title, description, build, state, visibility, defaults, tags,
	COALESCE(published_at,0), created_at, updated_at`

func scanSnapshot(row interface{ Scan(...any) error }) (Snapshot, error) {
	var s Snapshot
	err := row.Scan(&s.ID, &s.Title, &s.Description, &s.Build, &s.State, &s.Visibility,
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

// execer is the part of pgx these writes need, so one can be run either on the
// pool or inside a caller's transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PutSource inserts or updates a source by id.
func (s *Store) PutSource(ctx context.Context, src Source) error {
	return s.putSource(ctx, s.pool, src)
}

func (s *Store) putSource(ctx context.Context, db execer, src Source) error {
	if src.ID == "" || src.Name == "" || src.Version == "" {
		return errors.New("source needs id, name and version")
	}
	if src.TOML == "" {
		return fmt.Errorf("source %q has no TOML manifest", src.ID)
	}
	if src.Visibility == "" {
		// Default closed. Publishing something restricted is a disclosure that
		// cannot be undone; a restricted source nobody can see is a support
		// request. The costs are not symmetric.
		src.Visibility = VisibilityRestricted
	}
	if src.IndexStatus == "" {
		src.IndexStatus = "indexed"
	}
	now := s.nowFn()
	_, err := db.Exec(ctx, `
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
	// At most one reference, and every source that needs one has it. Checked
	// here for the same reason the assembly is: this is the single choke point
	// every snapshot passes through, curated and ad-hoc alike.
	pinned, err := s.sourcesByID(ctx, sourceIDs)
	if err != nil {
		return err
	}
	if err := checkReferences(pinned); err != nil {
		return fmt.Errorf("snapshot %q: %w", snap.ID, err)
	}
	// A gene list resolves variants to genes through a GTF it names, and varhub
	// looks that up *within the snapshot*. Pinning one without its gene model
	// produces a snapshot that loads and then fails every job with "gtf X is not
	// a GTF source in this snapshot" — caught here instead, where whoever is
	// assembling it can add the missing source.
	if missing := missingGeneListGTF(pinned); len(missing) > 0 {
		var parts []string
		for list, want := range missing {
			parts = append(parts, fmt.Sprintf("%s needs %q", list, want))
		}
		sort.Strings(parts)
		return fmt.Errorf("snapshot %q: %s; pin the gene model alongside the gene list",
			snap.ID, strings.Join(parts, "; "))
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
	// An ad-hoc snapshot is assembled per job and has nobody to ask which
	// reference to use, so it reaches for the assembly's default when something
	// in the selection needs one. The snapshot still *pins* it, so re-running
	// this later cannot drift onto a newer genome — the default is a choice made
	// once, not an indirection resolved every time.
	sourceIDs, err := s.withDefaultReference(ctx, build, sourceIDs)
	if err != nil {
		return "", err
	}
	// And the gene model a chosen gene list resolves through, for the same
	// reason: somebody picking "cancer genes" is asking for the answer, not for
	// a lesson in how it is computed.
	sourceIDs, err = s.withGeneListGTF(ctx, sourceIDs)
	if err != nil {
		return "", err
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

// SetSourceVisibility changes who may use a source.
//
// Its own write rather than part of the manifest update, because it is an access
// decision rather than a statement about the data — and because the manifest
// update refuses a source that a snapshot pins, which must not stand between an
// administrator and closing something off.
func (s *Store) SetSourceVisibility(ctx context.Context, id, level string) (Source, error) {
	if !ValidVisibility(level) {
		return Source{}, fmt.Errorf("unknown visibility %q", level)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE source SET visibility=$2, updated_at=$3 WHERE id=$1 RETURNING `+sourceCols,
		id, level, s.nowFn())
	src, err := scanSource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, fmt.Errorf("source %q: %w", id, ErrNotFound)
	}
	return src, err
}

// SetSnapshotVisibility changes a snapshot's own level.
//
// Only ever a floor: what a snapshot is actually offered at is the most
// restrictive of this and every source it pins. See Snapshot.EffectiveVisibility.
func (s *Store) SetSnapshotVisibility(ctx context.Context, id, level string) error {
	if !ValidVisibility(level) {
		return fmt.Errorf("unknown visibility %q", level)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE snapshot SET visibility=$2, updated_at=$3 WHERE id=$1`, id, level, s.nowFn())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("snapshot %q: %w", id, ErrNotFound)
	}
	return nil
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

// sourcesByID loads sources in one query, for the checks PutSnapshot runs.
func (s *Store) sourcesByID(ctx context.Context, ids []string) ([]Source, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sourceCols+` FROM source WHERE id = ANY($1)`, ids)
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

// withDefaultReference appends the assembly's default reference when the
// selection needs one and has none.
//
// Returns the ids unchanged when nothing requires a reference: most annotation
// needs no genome, and pinning one anyway would make every ad-hoc snapshot
// depend on a file it never opens.
func (s *Store) withDefaultReference(ctx context.Context, build string,
	sourceIDs []string) ([]string, error) {

	chosen, err := s.sourcesByID(ctx, sourceIDs)
	if err != nil {
		return nil, err
	}
	needs := false
	for _, src := range chosen {
		if src.IsReference() {
			return sourceIDs, nil // already pinned explicitly
		}
		if src.RequiresReference() {
			needs = true
		}
	}
	if !needs {
		return sourceIDs, nil
	}

	def, ok, err := s.DefaultReference(ctx, build)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Named plainly, because the fix is an administrator action rather than
		// anything the caller can do differently.
		var needy []string
		for _, src := range chosen {
			if src.RequiresReference() {
				needy = append(needy, src.Ref())
			}
		}
		return nil, fmt.Errorf("%v requires a reference genome, and %s has no default; "+
			"register a reference source for %s and mark it default",
			needy, build, build)
	}
	return append(slices.Clone(sourceIDs), def.ID), nil
}

// SourceInUse returns the named snapshots pinning a source — drafts and
// published ones, never ad-hoc.
//
// Ad-hoc snapshots are excluded deliberately, on the same reasoning DeleteSource
// uses: they are regenerable, the same selection produces the same id, and past
// results are self-contained because they carry their own column model. Counting
// them would make a source uneditable the moment anyone annotated with it, which
// is every source that has ever been useful.
func (s *Store) SourceInUse(ctx context.Context, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sn.id FROM snapshot sn
		  JOIN snapshot_source ss ON ss.snapshot_id = sn.id
		 WHERE ss.source_id = $1 AND sn.state <> $2
		 ORDER BY sn.id`, id, StateAdhoc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// UpdateSourceTOML replaces a source's manifest in place.
//
// The point is to fix a manifest without re-fetching what it describes. A
// missing requires_reference, a wrong annotation name, a changed prefix — none
// of those are reasons to download a source again, and for something like VEP
// that is hours.
//
// So the download state is carried over rather than taken from the new manifest:
// index_status stays as it was, and the rows recording which files are stored
// where are untouched. is_default_reference is not in the upsert at all, so it
// survives on its own.
//
// Refused when a named snapshot pins the source. A published snapshot is a
// promise about what an annotation ran against, and rewriting the manifest under
// it would change the meaning of results already returned. Ad-hoc snapshots
// pinning it are dropped instead of blocking, exactly as DeleteSource does.
//
// Identity may not change. A source's files are stored under its name and
// version, so a manifest that renames it describes something else — and the
// stored files would silently belong to neither.
func (s *Store) UpdateSourceTOML(ctx context.Context, id string, next Source) (Source, error) {
	if next.ID != id {
		return Source{}, fmt.Errorf("this manifest describes %s, not %s: "+
			"a source's files are stored under its name and version, so changing "+
			"either makes it a different source — register that one instead", next.ID, id)
	}
	inUse, err := s.SourceInUse(ctx, id)
	if err != nil {
		return Source{}, err
	}
	if len(inUse) > 0 {
		return Source{}, fmt.Errorf("%s is pinned by %v; a snapshot is a promise about "+
			"what an annotation ran against, so its sources cannot be rewritten underneath "+
			"it — remove it from those snapshots, or register a new version", id, inUse)
	}

	prev := s.pool.QueryRow(ctx, `SELECT `+sourceCols+` FROM source WHERE id=$1`, id)
	existing, err := scanSource(prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, fmt.Errorf("source %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Source{}, err
	}
	// What the deployment learned by provisioning, which the manifest does not
	// know and must not overwrite.
	next.IndexStatus = existing.IndexStatus

	// Likewise who may see it: an access decision somebody made deliberately, and
	// nothing a manifest says about the data should undo it. An edit that means to
	// change it says so; silence means keep.
	//
	// This is what made editing a manifest silently close a public source. The
	// editor posts only the TOML, the request carried no visibility, and the
	// default landed on the row — so an unrelated one-line change took a source
	// away from every anonymous caller using it, with nothing in the UI to say so.
	if next.Visibility == "" {
		next.Visibility = existing.Visibility
	}

	// One transaction, because the three writes are one fact: this source now
	// emits something different. Applied piecemeal, a failure between them leaves
	// a manifest live beside cached values computed from the manifest it
	// replaced — which is not a stale cache but a wrong one, and it looks exactly
	// like a correct one.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Source{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Regenerable, and now describing a manifest that no longer exists.
	if _, err := tx.Exec(ctx, `
		DELETE FROM snapshot
		 WHERE state = $1
		   AND id IN (SELECT snapshot_id FROM snapshot_source WHERE source_id = $2)`,
		StateAdhoc, id); err != nil {
		return Source{}, err
	}
	if err := s.putSource(ctx, tx, next); err != nil {
		return Source{}, err
	}
	// The annotation cache keys a source by "name:version", and neither has
	// changed — this edit is refused outright if they had. So nothing about the
	// key says the stored values are now answers to a different question: which
	// fields the source emits, and where each reads from, are exactly what a
	// manifest edit changes. A revision counter in the key would express that,
	// at the cost of a column every writer has to keep true; discarding what
	// cannot be trusted is the same outcome for the price of recomputing it.
	if err := anncache.PurgeSource(ctx, tx, existing.Ref()); err != nil {
		return Source{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Source{}, err
	}
	return next, nil
}
