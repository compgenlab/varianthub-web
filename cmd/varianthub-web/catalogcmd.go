package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/config"
)

// Catalog administration from the command line.
//
// The design's admin UI (register a source, build a snapshot, grant access) needs
// endpoints that do not exist yet. These subcommands drive the same catalog store
// those endpoints will, so an operator can populate a deployment today and the UI
// is additive later rather than a prerequisite.

// stringList collects a repeatable, comma-separable flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func openCatalog(ctx context.Context, cfg *config.Config) (*catalog.Store, error) {
	return catalog.Open(ctx, cfg.DatabaseURL)
}

// cmdSource handles `source add|list`.
func cmdSource(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: source <add|list>")
	}
	cat, err := openCatalog(ctx, cfg)
	if err != nil {
		return err
	}
	defer cat.Close()

	switch args[0] {
	case "list":
		srcs, err := cat.ListSources(ctx)
		if err != nil {
			return err
		}
		if len(srcs) == 0 {
			fmt.Println("(no sources registered)")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tREF\tKIND\tACCESS\tINDEX\tTITLE")
		for _, s := range srcs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				s.ID, s.Ref(), s.Kind, s.Visibility, s.IndexStatus, s.Title)
		}
		return w.Flush()

	case "add":
		fs := flag.NewFlagSet("source add", flag.ContinueOnError)
		id := fs.String("id", "", "catalog id (default: <name>-<version> from the TOML)")
		title := fs.String("title", "", "display title (default: the TOML's title)")
		detail := fs.String("detail", "", "one-line description for the picker")
		private := fs.Bool("private", false, "mark the source private (needs a team grant)")
		origin := fs.String("origin", "", "provenance note, e.g. \"registry: ncbi-clinvar\"")
		building := fs.Bool("building", false, "mark the index as still being built")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rest := fs.Args()
		if len(rest) != 1 {
			return fmt.Errorf("usage: source add [flags] <source.toml>\n\n" +
				"The file is a varhub source fragment — a single [[sources]] entry, " +
				"exactly as `varhub source add` writes it under annotations/sources/.")
		}

		body, err := os.ReadFile(rest[0])
		if err != nil {
			return fmt.Errorf("read %s: %w", rest[0], err)
		}
		// Derive the projection columns from the manifest; the text itself is
		// stored verbatim and is what varhub actually reads.
		src, err := catalog.SourceFromTOML(string(body))
		if err != nil {
			return fmt.Errorf("%s: %w", rest[0], err)
		}
		if *id != "" {
			src.ID = *id
		}
		if *title != "" {
			src.Title = *title
		}
		if *detail != "" {
			src.Detail = *detail
		}
		if *private {
			src.Visibility = catalog.VisibilityPrivate
		}
		if *origin != "" {
			src.Origin = *origin
		}
		if *building {
			src.IndexStatus = "building"
		}
		if err := cat.PutSource(ctx, src); err != nil {
			return err
		}
		fmt.Printf("registered source %s (%s, kind=%s)\n", src.ID, src.Ref(), src.Kind)
		fmt.Printf("add it to a snapshot with:\n  varianthub-web snapshot add <name> --build %s --source %s\n",
			"GRCh38", src.ID)
		return nil

	default:
		return fmt.Errorf("unknown source subcommand %q (want add|list)", args[0])
	}
}

// cmdSnapshot handles `snapshot add|list`.
func cmdSnapshot(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: snapshot <add|list>")
	}
	cat, err := openCatalog(ctx, cfg)
	if err != nil {
		return err
	}
	defer cat.Close()

	switch args[0] {
	case "list":
		snaps, err := cat.ListSnapshots(ctx)
		if err != nil {
			return err
		}
		if len(snaps) == 0 {
			fmt.Println("(no snapshots)")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tBUILD\tSTATE\tSOURCES\tDEFAULTS\tTITLE")
		for _, s := range snaps {
			// Sources are not loaded by List; fetch for an accurate count.
			n := 0
			if full, err := cat.GetSnapshot(ctx, s.ID); err == nil {
				n = len(full.Sources)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
				s.ID, s.Build, s.State, n, strings.Join(s.Defaults, ","), s.Title)
		}
		return w.Flush()

	case "add":
		fs := flag.NewFlagSet("snapshot add", flag.ContinueOnError)
		build := fs.String("build", "", "genome assembly, e.g. GRCh38 (required)")
		title := fs.String("title", "", "display title")
		desc := fs.String("desc", "", "description")
		publish := fs.Bool("publish", false, "publish immediately (default: draft)")
		var sources, defaults, tags stringList
		fs.Var(&sources, "source", "source id to pin (repeatable/comma-separated)")
		fs.Var(&defaults, "default", "annotation applied by default (repeatable/comma-separated)")
		fs.Var(&tags, "tag", "display tag (repeatable/comma-separated)")
		if len(args) < 2 {
			return fmt.Errorf("usage: snapshot add <name> --build B --source S [--source S...] " +
				"[--default ann] [--title T] [--publish]")
		}
		name := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *build == "" {
			return fmt.Errorf("--build is required (the assembly, e.g. GRCh38)")
		}
		if len(sources) == 0 {
			return fmt.Errorf("--source is required: a snapshot with no sources cannot annotate")
		}

		state := catalog.StateDraft
		if *publish {
			state = catalog.StatePublished
		}
		if err := cat.PutSnapshot(ctx, catalog.Snapshot{
			ID: name, Title: *title, Description: *desc,
			Build: *build, State: state,
			Defaults: defaults, Tags: tags,
		}, sources); err != nil {
			return err
		}
		// Reading it back proves the pins resolved and the manifest will
		// materialize — a snapshot that fails here would fail at job time instead.
		full, err := cat.GetSnapshot(ctx, name)
		if err != nil {
			return fmt.Errorf("snapshot written but does not load: %w", err)
		}
		fmt.Printf("snapshot %q (%s, %s) pins %d source(s):\n",
			full.ID, full.Build, full.State, len(full.Sources))
		for _, s := range full.Sources {
			fmt.Printf("  %s\n", s.Ref())
		}
		if state == catalog.StateDraft {
			fmt.Println("\nIt is a draft — pass --publish to make it selectable in the UI.")
		}
		return nil

	default:
		return fmt.Errorf("unknown snapshot subcommand %q (want add|list)", args[0])
	}
}
