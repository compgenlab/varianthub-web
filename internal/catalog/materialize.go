package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	cacheDir, err := m.Store.StorageForSources(ctx, ids)
	if err != nil {
		return "", nil, err
	}
	if cacheDir == "" {
		// Nothing downloaded yet. Use the default location so varhub looks where
		// a download would have put it, and reports the absence itself.
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

	if err := m.writeWithCache(dir, snap, cacheDir); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func (m *Materializer) write(dir string, snap Snapshot) error {
	return m.writeWithCache(dir, snap, m.CacheDir)
}

func (m *Materializer) writeWithCache(dir string, snap Snapshot, cacheDir string) error {
	writeFile := func(rel, body string) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0o600)
	}

	// config.toml. Absolute data/cache paths, so nothing resolves relative to the
	// ephemeral home and the shared cache is actually shared.
	cfg := fmt.Sprintf(`# Generated per job from the VariantHub catalog. Do not edit.
data_dir         = %s
cache_dir        = %s
annotations_dir  = "./annotations"
default_snapshot = %s
`, tomlString(m.DataDir), tomlString(cacheDir), tomlString(snap.ID))
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
		rel := filepath.Join("annotations", "sources", src.Name, src.Version,
			src.Name+"-"+src.Version+".toml")
		if err := writeFile(rel, src.TOML); err != nil {
			return fmt.Errorf("write source %s: %w", src.Ref(), err)
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
	cacheDir, err := m.Store.StorageForSources(ctx, sourceIDs)
	if err != nil {
		return "", nil, err
	}
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
	if err := m.writeWithCache(dir, snap, cacheDir); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}
