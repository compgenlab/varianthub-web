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

	dir, err := os.MkdirTemp(m.Root, "varhub-home-")
	if err != nil {
		return "", nil, fmt.Errorf("create annotation home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	if err := m.write(dir, snap); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func (m *Materializer) write(dir string, snap Snapshot) error {
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
`, tomlString(m.DataDir), tomlString(m.CacheDir), tomlString(snap.ID))
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
