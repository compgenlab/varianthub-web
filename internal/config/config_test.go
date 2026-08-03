package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write a config file and point VHW_CONFIG at it.
func withFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "varianthub-web.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VHW_CONFIG", path)
	return path
}

// The three layers, in order, on one setting each.
func TestPrecedence(t *testing.T) {
	withFile(t, `
[server]
  addr = ":9000"
[database]
  url = "postgres://from-file/db"
[worker]
  count = 7
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// From the file.
	if c.Addr != ":9000" || c.Workers != 7 {
		t.Errorf("file not applied: addr=%q workers=%d", c.Addr, c.Workers)
	}
	// From the defaults, untouched by the file.
	if c.RatePerMin != 30 {
		t.Errorf("default lost: rate_per_min=%d", c.RatePerMin)
	}

	// The environment wins over the file.
	t.Setenv("VHW_ADDR", ":7777")
	t.Setenv("VHW_WORKERS", "3")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":7777" || c.Workers != 3 {
		t.Errorf("env did not override the file: addr=%q workers=%d", c.Addr, c.Workers)
	}
}

// A blank variable is not an instruction to erase a configured value — that is
// how an unset-but-present env var in a compose file wipes a working setting.
func TestEmptyEnvDoesNotClobberTheFile(t *testing.T) {
	withFile(t, `
[database]
  url = "postgres://from-file/db"
[auth.cilogon]
  client_id = "configured-id"
`)
	t.Setenv("VHW_CILOGON_CLIENT_ID", "")
	t.Setenv("VHW_ADDR", "   ")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CILogonClientID != "configured-id" {
		t.Errorf("an empty env var erased the file value: %q", c.CILogonClientID)
	}
	if c.Addr != ":8080" {
		t.Errorf("a whitespace env var was taken literally: %q", c.Addr)
	}
}

// false and 0 are real values, and must survive being written in the file.
func TestZeroValuesAreDistinguishableFromAbsent(t *testing.T) {
	withFile(t, `
[database]
  url = "postgres://x/db"
[auth]
  allow_anonymous = true
[limits]
  rate_per_min = 0
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.AllowAnonymous {
		t.Error("allow_anonymous = true was not applied")
	}
	if c.RatePerMin != 0 {
		t.Errorf("rate_per_min = 0 was treated as absent: %d", c.RatePerMin)
	}
}

func TestDurationsAndLists(t *testing.T) {
	withFile(t, `
[database]
  url = "postgres://x/db"
[worker]
  job_timeout = "90m"
  job_ttl = "7d"
[storage]
  paths = ["fast=/mnt/fast", "bulk=/mnt/bulk"]
  s3 = ["cold=s3://vh-cold/prod"]
[auth.cilogon]
  auto_provision_domains = ["iu.edu", "example.org"]
`)
	_, err := Load()
	// "7d" is not a Go duration; the error must say which key, not just fail.
	if err == nil {
		t.Fatal("accepted an invalid duration")
	}
	if !strings.Contains(err.Error(), "job_ttl") {
		t.Errorf("error does not name the key: %v", err)
	}

	withFile(t, `
[database]
  url = "postgres://x/db"
[worker]
  job_timeout = "90m"
[storage]
  paths = ["fast=/mnt/fast", "bulk=/mnt/bulk"]
  s3 = ["cold=s3://vh-cold/prod"]
[auth.cilogon]
  auto_provision_domains = ["iu.edu", "example.org"]
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.JobTimeout != 90*time.Minute {
		t.Errorf("job_timeout = %v", c.JobTimeout)
	}
	if len(c.StoragePaths) != 2 || c.StoragePaths[0] != "fast=/mnt/fast" {
		t.Errorf("storage.paths = %v", c.StoragePaths)
	}
	if len(c.CILogonAutoProvision) != 2 {
		t.Errorf("auto_provision_domains = %v", c.CILogonAutoProvision)
	}

	// And the parsed storage locations still come out right.
	locs, err := c.StorageLocations()
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 3 || !locs[0].Default || locs[0].Name != "fast" {
		t.Errorf("StorageLocations = %+v", locs)
	}
	if locs[2].Kind != "s3" || locs[2].Path != "s3://vh-cold/prod" {
		t.Errorf("s3 location = %+v", locs[2])
	}
}

// A misspelled key that silently does nothing is the failure a config file is
// meant to prevent, so it stops startup.
func TestUnknownKeysAreRejected(t *testing.T) {
	withFile(t, `
[database]
  url = "postgres://x/db"
[worker]
  wrokers = 4
`)
	_, err := Load()
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "wrokers") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

// A named file that is not there is a mistake, not a reason to fall back.
func TestMissingExplicitConfigIsAnError(t *testing.T) {
	t.Setenv("VHW_CONFIG", filepath.Join(t.TempDir(), "nope.toml"))
	t.Setenv("VHW_DATABASE_URL", "postgres://x/db")
	if _, err := Load(); err == nil {
		t.Fatal("a missing VHW_CONFIG was ignored")
	}
}

// With no file at all, the environment alone still configures the service —
// existing deployments must not need one.
func TestEnvOnlyStillWorks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // so the relative search path finds nothing
	t.Setenv("VHW_CONFIG", "")
	t.Setenv("VHW_DATABASE_URL", "postgres://env/db")
	t.Setenv("VHW_ALLOW_ANONYMOUS", "true")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://env/db" || !c.AllowAnonymous {
		t.Errorf("env-only load: %+v", c)
	}
	if c.Addr != ":8080" {
		t.Errorf("defaults lost: %q", c.Addr)
	}
}

// Without a database there is nothing to run against; the message has to say
// where to put one.
func TestDatabaseIsRequired(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("VHW_CONFIG", "")
	t.Setenv("VHW_DATABASE_URL", "")
	_, err := Load()
	if err == nil {
		t.Fatal("started with no database configured")
	}
	if !strings.Contains(err.Error(), "database.url") || !strings.Contains(err.Error(), "VHW_DATABASE_URL") {
		t.Errorf("error names neither way to set it: %v", err)
	}
}

// The shipped example must load, and must agree with the defaults it claims to
// document. Without this the file drifts silently and becomes advice that is
// quietly wrong — the failure mode of every example config ever written.
func TestExampleFileIsValid(t *testing.T) {
	body, err := os.ReadFile("../../varianthub-web.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	withFile(t, string(body))
	c, err := Load()
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}
	// And it must agree with the defaults, or the documentation lies.
	d := Defaults()
	if c.Addr != d.Addr || c.Workers != d.Workers || c.RatePerMin != d.RatePerMin ||
		c.JobTimeout != d.JobTimeout || c.MaxUploadBytes != d.MaxUploadBytes {
		t.Errorf("the example disagrees with Defaults():\n example: %+v\ndefaults: %+v", c, d)
	}
	if len(c.TrustedProxy) != len(d.TrustedProxy) {
		t.Errorf("trusted_proxies: example %v, defaults %v", c.TrustedProxy, d.TrustedProxy)
	}
}

// redactDSN decides whether a file holds a secret, so getting it wrong either
// warns about every file or about none.
func TestRedactDSN(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:p@h:5432/db":  "postgres://u:***@h:5432/db",
		"postgres://u@h/db":         "postgres://u@h/db",
		"postgres://h/db":           "postgres://h/db",
		"":                          "",
		"not-a-url":                 "not-a-url",
		"postgres://u:p@h/db?x=y@z": "postgres://u:***@h/db?x=y@z",
	} {
		if got := redactDSN(in); got != want {
			t.Errorf("redactDSN(%q) = %q, want %q", in, got, want)
		}
	}
}

// The permission warning is conditioned on the file holding a secret, so a
// config of plain settings does not train people to ignore it.
func TestFileSecretDetection(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"no secrets", "[database]\n  url = \"postgres://user@host/db\"\n", false},
		{"password in the DSN", "[database]\n  url = \"postgres://user:pw@host/db\"\n", true},
		{"client secret", "[auth.cilogon]\n  client_secret = \"s\"\n", true},
		{"empty file", "# nothing\n", false},
		// The compose case: the file names no secret and the DSN arrives in the
		// environment. Blaming the file for that is a false alarm.
		{"empty url, secret comes from env", "[database]\n  url = \"\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := applyFile(Defaults(), path)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("fileHasSecret = %v, want %v", got, tc.want)
			}

			var warned bool
			warnIfWorldReadable(path, got, func(string, ...any) { warned = true })
			if warned != tc.want {
				t.Errorf("warned = %v, want %v", warned, tc.want)
			}
		})
	}

	// A 0600 file never warns, secret or not.
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned bool
	warnIfWorldReadable(path, true, func(string, ...any) { warned = true })
	if warned {
		t.Error("warned about a 0600 file")
	}
}

// Provisioning gets its own bound, and an existing deployment that set only
// job_timeout keeps the behaviour it had.
func TestDownloadTimeout(t *testing.T) {
	withFile(t, `
[database]
  url = "postgres://x/db"
[worker]
  job_timeout = "30m"
  download_timeout = "6h"
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.JobTimeout != 30*time.Minute || c.DownloadTimeout != 6*time.Hour {
		t.Errorf("job=%v download=%v", c.JobTimeout, c.DownloadTimeout)
	}

	// The default is longer than an annotation's, which is the whole point.
	d := Defaults()
	if d.DownloadTimeout <= d.JobTimeout {
		t.Errorf("download timeout %v is not longer than job timeout %v",
			d.DownloadTimeout, d.JobTimeout)
	}
}
