package config

import "testing"

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
