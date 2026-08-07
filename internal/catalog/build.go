package catalog

import (
	"context"
	"fmt"
	"strings"
)

// Build is a genome assembly the installation offers.
//
// Keyed by the assembly name itself, because that name is already the join: a
// source declares `assembly = "GRCh38"`, a snapshot declares a build, and a
// reference is chosen per assembly. Adding a surrogate id would create a second
// identity for the same thing and an opportunity for them to disagree.
type Build struct {
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	SortOrder   int    `json:"sort_order"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`

	// Sources counts what is registered against this build. Populated by
	// ListBuilds so the admin view can say what removing one would orphan, and
	// so the annotation form can skip a build with nothing behind it.
	Sources int `json:"sources"`
}

// ListBuilds returns every build, in picker order, with a count of the sources
// registered against each.
func (s *Store) ListBuilds(ctx context.Context) ([]Build, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.name, b.label, b.description, b.sort_order, b.created_at, b.updated_at,
		       (SELECT count(*) FROM source src WHERE src.build = b.name)
		  FROM build b
		 ORDER BY b.sort_order, b.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		var b Build
		if err := rows.Scan(&b.Name, &b.Label, &b.Description, &b.SortOrder,
			&b.CreatedAt, &b.UpdatedAt, &b.Sources); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PutBuild adds or updates a build.
func (s *Store) PutBuild(ctx context.Context, b Build) error {
	name := strings.TrimSpace(b.Name)
	if name == "" {
		return fmt.Errorf("a build needs a name")
	}
	// Whitespace inside would make the name unmatchable against a manifest's
	// assembly, which is the whole point of the record.
	if strings.ContainsAny(name, " \t\n") {
		return fmt.Errorf("build %q: a name cannot contain whitespace — it must match "+
			"a source's assembly exactly", name)
	}
	now := s.nowFn()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO build (name, label, description, sort_order, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$5)
		ON CONFLICT (name) DO UPDATE
		   SET label       = excluded.label,
		       description = excluded.description,
		       sort_order  = excluded.sort_order,
		       updated_at  = excluded.updated_at`,
		name, strings.TrimSpace(b.Label), strings.TrimSpace(b.Description), b.SortOrder, now)
	return err
}

// DeleteBuild removes a build, refusing while anything still declares it.
//
// Refused rather than cascaded, like deleting a source a snapshot pins: the
// sources and snapshots would keep their assembly strings and keep working,
// while the picker silently stopped offering the build they need — a state
// that is hard to notice and harder to explain.
func (s *Store) DeleteBuild(ctx context.Context, name string) error {
	var sources, snapshots int
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM source WHERE build = $1),
		       (SELECT count(*) FROM snapshot WHERE build = $1)`, name).
		Scan(&sources, &snapshots); err != nil {
		return err
	}
	if sources > 0 || snapshots > 0 {
		return fmt.Errorf("%s is still used by %d source(s) and %d snapshot(s); "+
			"remove or re-assign those first", name, sources, snapshots)
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM build WHERE name=$1`, name)
	return err
}
