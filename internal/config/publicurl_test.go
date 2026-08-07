package config

import (
	"os"
	"path/filepath"
	"testing"
)

// One hostname, everything derived. The domain was otherwise written three
// times — CORS, the CILogon callback, and the address a generated client calls —
// and three copies are three chances to move one and forget another.
func TestPublicURLFillsInWhatItImplies(t *testing.T) {
	c := Defaults()
	c.PublicURL = "https://varianthub.compgenlab.org"
	c.applyPublicURL()

	if len(c.CORSOrigins) != 1 || c.CORSOrigins[0] != "https://varianthub.compgenlab.org" {
		t.Errorf("CORS origins = %v", c.CORSOrigins)
	}
	if want := "https://varianthub.compgenlab.org/auth/cilogon/callback"; c.CILogonRedirectURL != want {
		t.Errorf("callback = %q, want %q", c.CILogonRedirectURL, want)
	}
}

// A trailing slash must not produce a doubled one in the derived callback,
// which CILogon would reject as a mismatch against the registered URL.
func TestPublicURLTrailingSlashIsTrimmed(t *testing.T) {
	c := Defaults()
	c.PublicURL = "https://varianthub.compgenlab.org/"
	c.applyPublicURL()

	if c.PublicURL != "https://varianthub.compgenlab.org" {
		t.Errorf("PublicURL = %q", c.PublicURL)
	}
	if got := c.CILogonRedirectURL; got != "https://varianthub.compgenlab.org/auth/cilogon/callback" {
		t.Errorf("callback = %q", got)
	}
}

// An explicit setting wins. A deployment that names its own origins or callback
// has a reason to, and a default that overruled it would be a setting that does
// nothing.
func TestPublicURLDoesNotOverruleExplicitSettings(t *testing.T) {
	c := Defaults()
	c.PublicURL = "https://varianthub.compgenlab.org"
	c.CORSOrigins = []string{"http://localhost:5173"}
	c.CILogonRedirectURL = "https://proxy.example.org/cb"
	c.applyPublicURL()

	if len(c.CORSOrigins) != 1 || c.CORSOrigins[0] != "http://localhost:5173" {
		t.Errorf("explicit CORS origins were replaced: %v", c.CORSOrigins)
	}
	if c.CILogonRedirectURL != "https://proxy.example.org/cb" {
		t.Errorf("explicit callback was replaced: %q", c.CILogonRedirectURL)
	}
}

// With no public URL nothing is invented: a dev stack has no canonical host.
func TestPublicURLAbsentDerivesNothing(t *testing.T) {
	c := Defaults()
	c.applyPublicURL()
	if len(c.CORSOrigins) != 0 || c.CILogonRedirectURL != "" {
		t.Errorf("derived %v / %q from no public URL", c.CORSOrigins, c.CILogonRedirectURL)
	}
}

// Everything settable by environment is settable in the file, which is what a
// deployment that keeps its configuration in git actually uses. A setting
// reachable only through an env var would be one the config file cannot express.
func TestPublicURLComesFromTheFileToo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "varianthub-web.toml")
	if err := os.WriteFile(path, []byte(`
[server]
addr = "10.0.0.5:8080"
public_url = "https://varianthub.compgenlab.org"

[database]
url = "postgres://example/db"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	c := Defaults()
	if _, err := applyFile(c, path); err != nil {
		t.Fatal(err)
	}
	c.applyPublicURL()

	if c.Addr != "10.0.0.5:8080" {
		t.Errorf("addr = %q; the listen endpoint is not file-settable", c.Addr)
	}
	if c.DatabaseURL != "postgres://example/db" {
		t.Errorf("database url = %q; it is not file-settable", c.DatabaseURL)
	}
	if c.PublicURL != "https://varianthub.compgenlab.org" {
		t.Errorf("public_url = %q; it is not file-settable", c.PublicURL)
	}
	// And the derivations still happen from a file-supplied value.
	if len(c.CORSOrigins) != 1 || c.CILogonRedirectURL == "" {
		t.Errorf("nothing derived from the file's public_url: %v / %q",
			c.CORSOrigins, c.CILogonRedirectURL)
	}
}
