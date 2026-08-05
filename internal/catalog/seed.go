package catalog

import (
	"context"
	"log"
)

// builtinsTOML is the varhub built-in annotator source. It needs no downloaded
// data, which makes it the one source a dev stack can rely on offline.
const builtinsTOML = `[[sources]]
  type    = "builtin"
  name    = "builtins"
  version = "1"

  [[sources.annotations]]
    builtin = "auto_id"
    name    = "auto_id"

  [[sources.annotations]]
    builtin = "tstv"
    name    = "tstv"

  [[sources.annotations]]
    builtin = "indel"
    name    = "indel"
`

// Seed inserts a starter snapshot if the catalog is empty, so a fresh stack can
// annotate immediately. It is a no-op once anything exists — it will not
// overwrite a real catalog, which is what makes it safe to run on every start.
func (s *Store) Seed(ctx context.Context) error {
	// The default registry is seeded independently of the starter snapshot: a
	// deployment with a real catalog still wants somewhere to import from.
	if err := s.SeedRegistry(ctx); err != nil {
		return err
	}
	snapshots, sources, err := s.Count(ctx)
	if err != nil {
		return err
	}
	if snapshots > 0 || sources > 0 {
		log.Printf("seed: catalog already populated (%d snapshot(s), %d source(s)); nothing to do",
			snapshots, sources)
		return nil
	}

	if err := s.PutSource(ctx, Source{
		ID:      "builtins",
		Name:    "builtins",
		Version: "1",
		Title:   "Built-in annotators",
		Detail:  "auto_id, tstv, indel — computed from the variant, no data files",
		Kind:    "builtin",
		// Stated rather than inherited: sources default to private now, and a
		// starter catalog whose only source nobody can see is a broken install.
		// A builtin computes from the variant and discloses nothing.
		Visibility: VisibilityPublic,
		Origin:     "built in to varhub",
		TOML:       builtinsTOML,
	}); err != nil {
		return err
	}

	if err := s.PutSnapshot(ctx, Snapshot{
		ID:          "dev",
		Title:       "Dev starter snapshot",
		Description: "Built-in annotators only — no downloaded data required.",
		Build:       "GRCh38",
		State:       StatePublished,
		Defaults:    []string{"auto_id", "tstv", "indel"},
		Tags:        []string{"builtins"},
	}, []string{"builtins"}); err != nil {
		return err
	}

	log.Printf("seed: created snapshot %q with 1 source", "dev")
	return nil
}
