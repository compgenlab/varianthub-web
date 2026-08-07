package catalog

import (
	"context"
	"fmt"

	"github.com/BurntSushi/toml"
)

// Reference genomes are ordinary sources (type = "reference"), pinned by a
// snapshot so a run stays reproducible when a newer patch release appears.
//
// What is specific to them lives here: which one an ad-hoc snapshot reaches for,
// and the two rules a snapshot has to satisfy — at most one reference, and every
// source that requires one has it.

// IsReference reports whether a source is a reference genome.
func (s Source) IsReference() bool { return s.Kind == "reference" }

// RequiresReference reports whether a source declares that it cannot work
// without one. Derived from the manifest on read, like the other projections, so
// a source registered before this existed needs no backfill.
func (s Source) RequiresReference() bool {
	var f struct {
		Sources []struct {
			RequiresReference bool `toml:"requires_reference"`
		} `toml:"sources"`
	}
	if _, err := toml.Decode(s.TOML, &f); err != nil || len(f.Sources) == 0 {
		return false
	}
	return f.Sources[0].RequiresReference
}

// SetDefaultReference marks a reference as the one ad-hoc snapshots pin for its
// assembly, clearing any previous default for that assembly.
//
// One per assembly, enforced by a partial unique index as well as here: two
// would make the ad-hoc choice arbitrary, and the arbitrariness would be
// invisible in the result.
func (s *Store) SetDefaultReference(ctx context.Context, sourceID string) error {
	src, err := s.GetSource(ctx, sourceID)
	if err != nil {
		return err
	}
	if !src.IsReference() {
		return fmt.Errorf("source %s is not a reference genome", src.Ref())
	}
	if src.Build == "" {
		return fmt.Errorf("reference %s declares no assembly", src.Ref())
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE source SET is_default_reference = FALSE WHERE build = $1 AND is_default_reference`,
		src.Build); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE source SET is_default_reference = TRUE, updated_at = $2 WHERE id = $1`,
		sourceID, s.nowFn()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DefaultReference returns the reference an ad-hoc snapshot should pin for an
// assembly.
//
// Assembly names are compared exactly and deliberately not normalized, as
// everywhere else here: a false match would annotate against the wrong
// coordinates and report nothing.
func (s *Store) DefaultReference(ctx context.Context, assembly string) (Source, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+sourceCols+` FROM source WHERE build = $1 AND is_default_reference LIMIT 1`,
		assembly)
	src, err := scanSource(row)
	if err != nil {
		return Source{}, false, nil
	}
	return src, true, nil
}

// checkReferences enforces the two snapshot rules.
//
// Both are checked when the snapshot is written rather than when a job runs, so
// the failure lands on whoever assembled it. Unmet, varhub renders {ref} as
// nothing and the tool dies on a mangled command line — VEP reports
// "Unexpected extra command-line parameter(s)" because --fasta swallows the next
// flag, which says nothing about a missing genome.
func checkReferences(sources []Source) error {
	var pinned *Source
	for i := range sources {
		if !sources[i].IsReference() {
			continue
		}
		if pinned != nil {
			return fmt.Errorf("a snapshot may pin one reference genome; this pins %s and %s",
				pinned.Ref(), sources[i].Ref())
		}
		pinned = &sources[i]
	}
	if pinned != nil {
		return nil
	}
	for _, src := range sources {
		if src.RequiresReference() {
			return fmt.Errorf("source %s requires a reference genome, but none is pinned",
				src.Ref())
		}
	}
	return nil
}
