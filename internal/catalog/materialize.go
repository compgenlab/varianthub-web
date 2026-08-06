package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Materializer builds a VARHUB_HOME on disk from catalog rows, so the worker can
// run `varhub -home <dir>` without the service holding any annotation config
// locally. It implements runner.HomeProvider.
//
// Only the *config tree* is per-job and ephemeral. DataDir and CacheDir point at
// shared, persistent storage: those hold downloaded source files and the tabix
// indexes built from them, and regenerating those per job would re-download
// gigabytes every time. Getting this backwards is the expensive mistake here.
type Materializer struct {
	Store *Store

	// DataDir and CacheDir are absolute paths to persistent storage, shared by
	// every job. Required.
	DataDir  string
	CacheDir string

	// Root is where per-job config trees are created (default: os.TempDir()).
	Root string

	// References maps an assembly to a reference FASTA on this worker. Written
	// into every job's config, because varhub resolves {ref} from the assembly
	// the snapshot declares — a tool step using {ref} gets an empty path
	// otherwise and fails inside the container with "no such file".
	References map[string]string
}

// Home materializes the snapshot's config tree into a fresh directory and
// returns it. cleanup removes the tree; the shared data and cache dirs are
// untouched.
func (m *Materializer) Home(ctx context.Context, snapshot string) (string, func(), error) {
	if m.Store == nil {
		return "", nil, fmt.Errorf("materializer has no catalog store")
	}
	if m.DataDir == "" || m.CacheDir == "" {
		return "", nil, fmt.Errorf("materializer needs DataDir and CacheDir")
	}
	if snapshot == "" {
		return "", nil, fmt.Errorf("no snapshot requested")
	}

	snap, err := m.Store.GetSnapshot(ctx, snapshot)
	if err != nil {
		return "", nil, err
	}

	// Read from wherever these sources were actually downloaded. The storage
	// location is the source cache; pointing a job at a different directory is
	// how you get "sources not downloaded" for data that is on disk.
	ids := make([]string, 0, len(snap.Sources))
	for _, src := range snap.Sources {
		ids = append(ids, src.ID)
	}
	// Where each source's files actually are. Sources in different locations are
	// normal, not an error: cache_dir carries the majority and the rest get a
	// location overlay beside their manifest.
	roots, err := m.Store.StorageRootsForSources(ctx, ids)
	if err != nil {
		return "", nil, err
	}
	// Helper scripts a build recipe names. They live in the catalog rather than
	// on this machine, so the worker has no other way to reach them.
	assets, err := m.Store.AssetsFor(ctx, ids)
	if err != nil {
		return "", nil, err
	}
	// What this deployment decided about these sources — output naming, and
	// whether a tool's setup output is published.
	settings, err := m.Store.SettingsFor(ctx, ids)
	if err != nil {
		return "", nil, err
	}
	cacheDir := commonRoot(roots)
	if cacheDir == "" {
		// Nothing downloaded yet, or no clear majority. Use the default location
		// so varhub looks where a download would have put it, and reports the
		// absence itself.
		if def, dErr := m.Store.DefaultStorage(ctx); dErr == nil {
			cacheDir = def.URI
		} else {
			cacheDir = m.CacheDir
		}
	}

	dir, err := os.MkdirTemp(m.Root, "varhub-home-")
	if err != nil {
		return "", nil, fmt.Errorf("create annotation home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	if err := m.writeWithCache(dir, snap, cacheDir, roots, assets, settings); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func (m *Materializer) write(dir string, snap Snapshot) error {
	return m.writeWithCache(dir, snap, m.CacheDir, nil, nil, nil)
}

func (m *Materializer) writeWithCache(dir string, snap Snapshot, cacheDir string,
	roots map[string]string, assets map[string][]Asset,
	settings map[string]SourceSettings) error {

	writeFile := func(rel, body string) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0o600)
	}
	// Assets are scripts a build step executes, so they need the mode to match.
	// Everything else here is data varhub reads, which stays 0600.
	writeExec := func(rel, body string) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0o700)
	}

	// config.toml. Absolute data/cache paths, so nothing resolves relative to the
	// ephemeral home and the shared cache is actually shared.
	cfg := fmt.Sprintf(`# Generated per job from the VariantHub catalog. Do not edit.
data_dir         = %s
cache_dir        = %s
annotations_dir  = "./annotations"
default_snapshot = %s
`, tomlString(m.DataDir), tomlString(cacheDir), tomlString(snap.ID))

	// Reference FASTAs, keyed by assembly. Written in sorted order so a given
	// deployment materializes the same file every time — a job home that differs
	// run to run is needlessly hard to compare.
	//
	// All of them, not just this snapshot's: the file is cheap, and selecting by
	// assembly here would duplicate a lookup varhub already does correctly.
	if len(m.References) > 0 {
		names := make([]string, 0, len(m.References))
		for name := range m.References {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			cfg += fmt.Sprintf("\n[references.%s]\n  fasta = %s\n",
				name, tomlString(m.References[name]))
		}
	}
	if err := writeFile("config.toml", cfg); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}

	// The snapshot manifest. Its name comes from the filename, so the file must be
	// <id>.toml; `title` and `default_annotations` are the manifest's key names.
	refs := make([]string, 0, len(snap.Sources))
	for _, src := range snap.Sources {
		refs = append(refs, src.Ref())
	}
	var manifest strings.Builder
	fmt.Fprintf(&manifest, "title       = %s\n", tomlString(snap.Title))
	if snap.Description != "" {
		fmt.Fprintf(&manifest, "description = %s\n", tomlString(snap.Description))
	}
	fmt.Fprintf(&manifest, "assembly    = %s\n", tomlString(snap.Build))
	fmt.Fprintf(&manifest, "sources     = %s\n", tomlStringList(refs))
	if len(snap.Defaults) > 0 {
		fmt.Fprintf(&manifest, "default_annotations = %s\n", tomlStringList(snap.Defaults))
	}
	if err := writeFile(filepath.Join("annotations", "snapshots", snap.ID+".toml"),
		manifest.String()); err != nil {
		return fmt.Errorf("write snapshot manifest: %w", err)
	}

	// Source fragments, verbatim. varhub resolves a "name:version" ref to
	// sources/<name>/<version>/<name>-<version>.toml, so the layout is fixed.
	for _, src := range snap.Sources {
		dir := filepath.Join("annotations", "sources", src.Name, src.Version)
		if err := writeFile(filepath.Join(dir, src.Name+"-"+src.Version+".toml"), src.TOML); err != nil {
			return fmt.Errorf("write source %s: %w", src.Ref(), err)
		}
		// Helper files the recipe names, beside the manifest where it looks for
		// them. Executable: an asset is a script a build step runs, and one
		// written 0600 fails at exec with a permission error that says nothing
		// about the cause.
		for _, a := range assets[src.ID] {
			if err := ValidateAssetName(a.Name); err != nil {
				return fmt.Errorf("source %s: %w", src.Ref(), err)
			}
			if err := writeExec(filepath.Join(dir, a.Name), a.Content); err != nil {
				return fmt.Errorf("write asset %s for %s: %w", a.Name, src.Ref(), err)
			}
		}
		// The overlay beside the manifest: everything this deployment knows
		// about the source that the source does not know about itself.
		//
		// A root when the source lives somewhere other than the job's cache_dir
		// — that is what lets one job read sources from different places, since
		// cache_dir names only one — plus whatever settings an administrator
		// has set for it.
		var body string
		if root := roots[src.ID]; root != "" && root != cacheDir {
			body += fmt.Sprintf("root = %s\n", tomlString(root))
		}
		if set := settings[src.ID]; !set.Empty() {
			if set.AnnotationPrefix != "" {
				body += fmt.Sprintf("annotation_prefix = %s\n", tomlString(set.AnnotationPrefix))
			}
			// varhub wants the destination, not a flag: an empty value means
			// "do not archive", so one field says both whether and where.
			//
			// This used to write `cache_setup = true`, for which varhub has no
			// field — an unknown key, ignored on parse. The setting read as
			// enabled in the UI and did nothing at all, so a 24-hour VEP install
			// finished with no archive and nothing to say why. Writing the
			// locator is what actually turns it on.
			//
			// The job's own storage target is the destination: it is where this
			// source's data is going anyway, so a deployment that can write the
			// data can write the archive beside it, with no second location to
			// configure or to get wrong. A source pinned to a different root
			// archives there instead, for the same reason.
			if set.CacheSetup {
				dest := cacheDir
				if root := roots[src.ID]; root != "" {
					dest = root
				}
				if dest != "" {
					body += fmt.Sprintf("tool_cache = %s\n", tomlString(dest))
				}
			}
		}
		// Written only when there is something to say. A file of nothing but a
		// header reads as an overlay whose content went missing.
		if body != "" {
			overlay := "# Generated per job from the VariantHub catalog. Do not edit.\n" + body
			if err := writeFile(filepath.Join(dir, src.Name+"-"+src.Version+".locations.toml"),
				overlay); err != nil {
				return fmt.Errorf("write locations for %s: %w", src.Ref(), err)
			}
		}
	}
	return nil
}

// tomlString quotes a Go string as a TOML basic string. Catalog values are
// admin-supplied, so a title containing a quote or backslash must not be able to
// produce a manifest that parses as something else.
func tomlString(s string) string { return strconv.Quote(s) }

func tomlStringList(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = strconv.Quote(it)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// HomeForSources materializes a home for an explicit set of sources, without
// persisting a snapshot.
//
// Provisioning is per source: a source is the unit of data, and requiring it to
// belong to a snapshot first would mean a newly registered source could not be
// downloaded until someone bundled it. The engine still needs a manifest, so one
// is synthesized in the temp home — but nothing is written to the catalog, since
// a download is not a reproducibility claim the way an annotation is.
func (m *Materializer) HomeForSources(ctx context.Context, sourceIDs []string) (string, func(), error) {
	if m.Store == nil {
		return "", nil, fmt.Errorf("materializer has no catalog store")
	}
	if m.DataDir == "" || m.CacheDir == "" {
		return "", nil, fmt.Errorf("materializer needs DataDir and CacheDir")
	}
	if len(sourceIDs) == 0 {
		return "", nil, fmt.Errorf("no sources selected")
	}

	all, err := m.Store.ListSources(ctx)
	if err != nil {
		return "", nil, err
	}
	byID := map[string]Source{}
	for _, s := range all {
		byID[s.ID] = s
	}

	snap := Snapshot{ID: "provision", Title: "Provisioning", State: StateAdhoc}
	for _, id := range sourceIDs {
		src, ok := byID[id]
		if !ok {
			return "", nil, fmt.Errorf("unknown source %q: %w", id, ErrNotFound)
		}
		// Every source in one manifest must agree on the assembly, or varhub
		// rejects the snapshot. Sources that declare none inherit whatever the
		// others state.
		if src.Build != "" {
			if snap.Build != "" && snap.Build != src.Build {
				return "", nil, fmt.Errorf(
					"cannot provision %s (%s) together with %s sources — "+
						"download one assembly at a time",
					src.Ref(), src.Build, snap.Build)
			}
			snap.Build = src.Build
		}
		snap.Sources = append(snap.Sources, src)
	}
	if snap.Build == "" {
		// No source declared one. varhub needs a value; the assembly is not
		// consulted while fetching, so any consistent choice works.
		snap.Build = "GRCh38"
	}

	// Resolve the cache the same way annotation does, so a home built for these
	// sources reads the directory a download wrote to. The download path
	// overrides this with the chosen target anyway; this matters for any other
	// caller of HomeForSources.
	roots, err := m.Store.StorageRootsForSources(ctx, sourceIDs)
	if err != nil {
		return "", nil, err
	}
	// This is the provisioning home — the one a download job runs in — so it is
	// the path that most needs a recipe's helper scripts present.
	assets, err := m.Store.AssetsFor(ctx, sourceIDs)
	if err != nil {
		return "", nil, err
	}
	settings, err := m.Store.SettingsFor(ctx, sourceIDs)
	if err != nil {
		return "", nil, err
	}
	cacheDir := commonRoot(roots)
	if cacheDir == "" {
		if def, dErr := m.Store.DefaultStorage(ctx); dErr == nil {
			cacheDir = def.URI
		} else {
			cacheDir = m.CacheDir
		}
	}

	dir, err := os.MkdirTemp(m.Root, "varhub-provision-")
	if err != nil {
		return "", nil, fmt.Errorf("create provisioning home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := m.writeWithCache(dir, snap, cacheDir, roots, assets, settings); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// commonRoot picks the location holding the most sources, which becomes the
// job's cache_dir. The rest get an overlay.
//
// Choosing the majority rather than the first keeps the overlay count down, and
// the choice is otherwise arbitrary — correctness does not depend on it, only
// the number of small files written into the job home.
func commonRoot(roots map[string]string) string {
	if len(roots) == 0 {
		return ""
	}
	count := map[string]int{}
	for _, r := range roots {
		if r != "" {
			count[r]++
		}
	}
	best, bestN := "", 0
	for r, n := range count {
		// Ties broken by name so a given catalog always materializes the same
		// way; a job home that differs run to run is needlessly hard to debug.
		if n > bestN || (n == bestN && r < best) {
			best, bestN = r, n
		}
	}
	return best
}
