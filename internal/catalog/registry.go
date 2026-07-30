package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/jackc/pgx/v5"
)

// DefaultRegistry is the public catalog varhub ships with. Seeded so a fresh
// deployment can import real sources without configuring anything.
const (
	DefaultRegistryID   = "public"
	DefaultRegistryName = "VariantHub public registry"
	DefaultRegistryURL  = "https://raw.githubusercontent.com/compgenlab/varianthub-public-data-registry/main/registry.toml"
)

// Registry is a configured source registry.
type Registry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Builtin   bool   `json:"builtin"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// RegistryEntry is one dataset offered by a registry. The TOML tags match the
// manifest varhub reads, so a registry published for the CLI works here as-is.
type RegistryEntry struct {
	Name          string `toml:"name" json:"name"`
	Version       string `toml:"version" json:"version,omitempty"`
	Title         string `toml:"title" json:"title,omitempty"`
	Assembly      string `toml:"assembly" json:"assembly,omitempty"`
	File          string `toml:"file" json:"file"`
	Description   string `toml:"description" json:"description,omitempty"`
	NonCommercial bool   `toml:"non_commercial" json:"non_commercial,omitempty"`
	Latest        bool   `toml:"latest" json:"latest,omitempty"`
}

// Ref is the "name:version" reference used to import.
func (e RegistryEntry) Ref() string {
	if e.Version == "" {
		return e.Name
	}
	return e.Name + ":" + e.Version
}

// RegistryManifest is a fetched registry.toml.
type RegistryManifest struct {
	Snapshots []RegistryEntry `toml:"snapshots" json:"snapshots"`
	Sources   []RegistryEntry `toml:"sources" json:"sources"`
}

// ListRegistries returns configured registries, the builtin default first.
func (s *Store) ListRegistries(ctx context.Context) ([]Registry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id,name,url,builtin,created_at,updated_at FROM registry
		 ORDER BY builtin DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registry{}
	for rows.Next() {
		var r Registry
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Builtin, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRegistry returns one configured registry.
func (s *Store) GetRegistry(ctx context.Context, id string) (Registry, error) {
	var r Registry
	err := s.pool.QueryRow(ctx,
		`SELECT id,name,url,builtin,created_at,updated_at FROM registry WHERE id=$1`, id).
		Scan(&r.ID, &r.Name, &r.URL, &r.Builtin, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registry{}, fmt.Errorf("registry %q: %w", id, ErrNotFound)
	}
	return r, err
}

// PutRegistry adds or updates a registry.
func (s *Store) PutRegistry(ctx context.Context, r Registry) error {
	if r.ID == "" || r.Name == "" {
		return errors.New("registry needs an id and a name")
	}
	if err := ValidateRegistryURL(r.URL); err != nil {
		return err
	}
	now := s.nowFn()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO registry (id,name,url,builtin,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$5)
		ON CONFLICT (id) DO UPDATE SET
		  name=excluded.name, url=excluded.url, updated_at=excluded.updated_at`,
		r.ID, r.Name, r.URL, r.Builtin, now)
	return err
}

// DeleteRegistry removes a configured registry. The builtin default cannot be
// deleted — it is restored on the next seed anyway, so removing it would only
// look like it worked.
func (s *Store) DeleteRegistry(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM registry WHERE id=$1 AND NOT builtin`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("registry %q: %w (or is the builtin default)", id, ErrNotFound)
	}
	return nil
}

// SeedRegistry inserts the public default if no registry is configured.
func (s *Store) SeedRegistry(ctx context.Context) error {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM registry`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return s.PutRegistry(ctx, Registry{
		ID: DefaultRegistryID, Name: DefaultRegistryName,
		URL: DefaultRegistryURL, Builtin: true,
	})
}

// ValidateRegistryURL rejects a location the server should not fetch.
//
// This is a guard rail, not a security boundary: only an admin can configure a
// registry, and an admin can already run code on a worker through a build recipe.
// It is here so a typo fails loudly rather than turning into a confusing fetch.
func ValidateRegistryURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid registry URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("registry URL must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("registry URL has no host")
	}
	return nil
}

// manifestURL appends registry.toml to a base URL, matching what the CLI does so
// the same location string works in both.
func manifestURL(loc string) string {
	if strings.HasSuffix(loc, ".toml") {
		return loc
	}
	return strings.TrimSuffix(loc, "/") + "/registry.toml"
}

// entryURL resolves an entry's `file` against the manifest's directory.
func entryURL(manifest, file string) (string, error) {
	u, err := url.Parse(manifestURL(manifest))
	if err != nil {
		return "", err
	}
	// The file path is registry-supplied. Resolving it against the manifest's
	// directory and then requiring the result to stay under that directory stops
	// a "../.." entry from reaching elsewhere on the host.
	base := *u
	base.Path = path.Dir(u.Path) + "/"
	rel, err := url.Parse(file)
	if err != nil {
		return "", fmt.Errorf("invalid file path %q: %w", file, err)
	}
	if rel.IsAbs() || rel.Host != "" {
		return "", fmt.Errorf("entry file must be a relative path, got %q", file)
	}
	out := base.ResolveReference(rel)
	if out.Host != u.Host || !strings.HasPrefix(out.Path, base.Path) {
		return "", fmt.Errorf("entry file %q escapes the registry directory", file)
	}
	return out.String(), nil
}

// registryClient bounds every fetch. A registry is a remote the operator pointed
// at; it must not be able to hang a request thread indefinitely.
var registryClient = &http.Client{Timeout: 20 * time.Second}

// maxRegistryBody caps a fetched document. Manifests and fragments are KB.
const maxRegistryBody = 4 << 20

func fetch(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain, application/toml, */*")
	res, err := registryClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", rawURL, res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxRegistryBody))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	return body, nil
}

// FetchManifest retrieves and parses a registry's catalog.
func FetchManifest(ctx context.Context, loc string) (*RegistryManifest, error) {
	if err := ValidateRegistryURL(loc); err != nil {
		return nil, err
	}
	body, err := fetch(ctx, manifestURL(loc))
	if err != nil {
		return nil, err
	}
	var m RegistryManifest
	if _, err := toml.Decode(string(body), &m); err != nil {
		return nil, fmt.Errorf("parse registry manifest: %w", err)
	}
	return &m, nil
}

// FetchEntry retrieves the config fragment for one entry, returning its TOML.
//
// The text is handed back for review rather than registered directly: an import
// is someone adopting a third party's manifest, and it will be executed by
// varhub. Seeing it first is the point.
func FetchEntry(ctx context.Context, loc string, e RegistryEntry) (string, error) {
	if e.File == "" {
		return "", fmt.Errorf("registry entry %q has no file", e.Ref())
	}
	target, err := entryURL(loc, e.File)
	if err != nil {
		return "", err
	}
	body, err := fetch(ctx, target)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// FindEntry locates a source entry by "name" or "name:version". A bare name
// resolves to the entry marked latest, matching the CLI — versions are not
// reliably sortable (semver 1.3, dbSNP b157, dates), so the publisher declares it.
func (m *RegistryManifest) FindEntry(ref string) (RegistryEntry, error) {
	name, version, hasVersion := strings.Cut(ref, ":")
	if version == "latest" {
		hasVersion = false
	}
	var fallback RegistryEntry
	var found bool
	for _, e := range m.Sources {
		if e.Name != name {
			continue
		}
		if hasVersion {
			if e.Version == version {
				return e, nil
			}
			continue
		}
		if e.Latest {
			return e, nil
		}
		if !found {
			fallback, found = e, true
		}
	}
	if found {
		return fallback, nil
	}
	return RegistryEntry{}, fmt.Errorf("registry has no source %q: %w", ref, ErrNotFound)
}
