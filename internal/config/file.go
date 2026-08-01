package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// The config file.
//
// Defaults, then the file, then the environment — each layer overriding the one
// before. The file is the readable record of how a deployment is set up; the
// environment stays authoritative on top of it so a container can override one
// value without templating a whole file, and so an existing env-only deployment
// keeps working with no file at all.
//
// Pointers on scalars are load-bearing: `workers = 0` and "workers not
// mentioned" are different statements, and a plain int cannot tell them apart.

// SearchPaths are tried in order when VHW_CONFIG is not set. A missing file is
// not an error — the environment alone is still a valid way to configure this.
var SearchPaths = []string{
	"varianthub-web.toml",
	"/etc/varianthub-web/config.toml",
}

type fileConfig struct {
	Server struct {
		Addr           string   `toml:"addr"`
		CORSOrigins    []string `toml:"cors_origins"`
		TrustedProxies []string `toml:"trusted_proxies"`
	} `toml:"server"`

	Database struct {
		URL string `toml:"url"`
	} `toml:"database"`

	Auth struct {
		AllowAnonymous *bool `toml:"allow_anonymous"`
		CILogon        struct {
			ClientID             string   `toml:"client_id"`
			ClientSecret         string   `toml:"client_secret"`
			RedirectURL          string   `toml:"redirect_url"`
			AutoProvisionDomains []string `toml:"auto_provision_domains"`
		} `toml:"cilogon"`
	} `toml:"auth"`

	Worker struct {
		Count      *int   `toml:"count"`
		VarhubBin  string `toml:"varhub_bin"`
		VarhubHome string `toml:"varhub_home"`
		DataDir    string `toml:"data_dir"`
		CacheDir   string `toml:"cache_dir"`
		JobTimeout string `toml:"job_timeout"`
		JobTTL     string `toml:"job_ttl"`
	} `toml:"worker"`

	Storage struct {
		Paths []string `toml:"paths"`
		S3    []string `toml:"s3"`
	} `toml:"storage"`

	Limits struct {
		RatePerMin     *int   `toml:"rate_per_min"`
		RateBurst      *int   `toml:"rate_burst"`
		MaxJobsPerIP   *int   `toml:"max_jobs_per_ip"`
		MaxUploadBytes *int64 `toml:"max_upload_bytes"`
		SubmitWaitCap  string `toml:"submit_wait_cap"`
	} `toml:"limits"`
}

// findConfigFile returns the path to load, or "" for none.
//
// An explicit VHW_CONFIG that does not exist is an error rather than a silent
// fallback: someone who named a file meant to use it, and starting anyway with
// defaults would look like the file was read.
func findConfigFile() (string, error) {
	if p := strings.TrimSpace(os.Getenv("VHW_CONFIG")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("VHW_CONFIG=%s: %w", p, err)
		}
		return p, nil
	}
	for _, p := range SearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", nil
}

// applyFile overlays a TOML file onto c.
//
// Unknown keys are an error. A config file exists so settings are visible and
// checkable; a misspelled key that silently does nothing is the exact failure it
// is supposed to prevent, and it is far cheaper to catch at startup than to
// discover from behaviour weeks later.
// It reports whether the file itself carries a secret, which is what decides
// whether its permissions are worth a warning.
func applyFile(c *Config, path string) (hasSecret bool, err error) {
	var f fileConfig
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return false, fmt.Errorf("%s: unknown setting(s): %s", path, strings.Join(keys, ", "))
	}
	hasSecret = f.Auth.CILogon.ClientSecret != "" ||
		redactDSN(f.Database.URL) != f.Database.URL

	setStr(&c.Addr, f.Server.Addr)
	setList(&c.CORSOrigins, f.Server.CORSOrigins)
	setList(&c.TrustedProxy, f.Server.TrustedProxies)

	setStr(&c.DatabaseURL, f.Database.URL)

	if f.Auth.AllowAnonymous != nil {
		c.AllowAnonymous = *f.Auth.AllowAnonymous
	}
	setStr(&c.CILogonClientID, f.Auth.CILogon.ClientID)
	setStr(&c.CILogonClientSecret, f.Auth.CILogon.ClientSecret)
	setStr(&c.CILogonRedirectURL, f.Auth.CILogon.RedirectURL)
	setList(&c.CILogonAutoProvision, f.Auth.CILogon.AutoProvisionDomains)

	setInt(&c.Workers, f.Worker.Count)
	setStr(&c.VarhubBin, f.Worker.VarhubBin)
	setStr(&c.VarhubHome, f.Worker.VarhubHome)
	setStr(&c.DataDir, f.Worker.DataDir)
	setStr(&c.CacheDir, f.Worker.CacheDir)
	if err := setDur(&c.JobTimeout, f.Worker.JobTimeout, path, "worker.job_timeout"); err != nil {
		return hasSecret, err
	}
	if err := setDur(&c.JobTTL, f.Worker.JobTTL, path, "worker.job_ttl"); err != nil {
		return hasSecret, err
	}

	setList(&c.StoragePaths, f.Storage.Paths)
	setList(&c.StorageS3, f.Storage.S3)

	setInt(&c.RatePerMin, f.Limits.RatePerMin)
	setInt(&c.RateBurst, f.Limits.RateBurst)
	setInt(&c.MaxJobsPerIP, f.Limits.MaxJobsPerIP)
	if f.Limits.MaxUploadBytes != nil {
		c.MaxUploadBytes = *f.Limits.MaxUploadBytes
	}
	if err := setDur(&c.SubmitWaitCap, f.Limits.SubmitWaitCap, path, "limits.submit_wait_cap"); err != nil {
		return hasSecret, err
	}
	return hasSecret, nil
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func setList(dst *[]string, v []string) {
	// An empty list in the file means "not stated", not "empty". Clearing a
	// defaulted list — trusted_proxies, say — is done by setting it to
	// something, not by declaring it empty, because the two are
	// indistinguishable in TOML once the key is absent.
	if len(v) > 0 {
		*dst = v
	}
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}

func setDur(dst *time.Duration, v, path, key string) error {
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %s = %q: %w", path, key, v, err)
	}
	*dst = d
	return nil
}

// warnIfWorldReadable notes a config file others can read *and* that has
// something worth reading.
//
// Conditioned on the file holding a secret, because most do not: a deployment
// that keeps its DSN and client secret in the environment has a config file of
// plain settings, and warning about its permissions every startup would train
// people to ignore the warning that matters. The judgement is on the file's own
// contents, not the resolved configuration — a password that arrived in an
// environment variable is not in the file, and blaming the file for it is the
// same false alarm in a different disguise.
//
// Not fatal either: refusing to start over a file mode would be its own outage.
func warnIfWorldReadable(path string, fileHasSecret bool, logf func(string, ...any)) {
	if !fileHasSecret {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if mode := info.Mode().Perm(); mode&0o044 != 0 {
		logf("config: %s holds a secret and is readable by group/other (%04o)"+
			" — consider chmod 600", path, mode)
	}
}

// Redacted renders the resolved configuration as TOML, with secrets masked.
//
// The point of a config file is being able to see how a deployment is set up.
// That is only true if you can see what it *resolved to* after the environment
// has had its say, which is what this prints.
func (c *Config) Redacted() string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("[server]")
	p("  addr = %q", c.Addr)
	p("  cors_origins = %s", tomlList(c.CORSOrigins))
	p("  trusted_proxies = %s", tomlList(c.TrustedProxy))
	p("")
	p("[database]")
	p("  url = %q", redactDSN(c.DatabaseURL))
	p("")
	p("[auth]")
	p("  allow_anonymous = %v", c.AllowAnonymous)
	p("")
	p("[auth.cilogon]")
	p("  client_id = %q", c.CILogonClientID)
	p("  client_secret = %q", mask(c.CILogonClientSecret))
	p("  redirect_url = %q", c.CILogonRedirectURL)
	p("  auto_provision_domains = %s", tomlList(c.CILogonAutoProvision))
	p("")
	p("[worker]")
	p("  count = %d", c.Workers)
	p("  varhub_bin = %q", c.VarhubBin)
	p("  varhub_home = %q", c.VarhubHome)
	p("  data_dir = %q", c.DataDir)
	p("  cache_dir = %q", c.CacheDir)
	p("  job_timeout = %q", c.JobTimeout.String())
	p("  job_ttl = %q", c.JobTTL.String())
	p("")
	p("[storage]")
	p("  paths = %s", tomlList(c.StoragePaths))
	p("  s3 = %s", tomlList(c.StorageS3))
	p("")
	p("[limits]")
	p("  rate_per_min = %d", c.RatePerMin)
	p("  rate_burst = %d", c.RateBurst)
	p("  max_jobs_per_ip = %d", c.MaxJobsPerIP)
	p("  max_upload_bytes = %d", c.MaxUploadBytes)
	p("  submit_wait_cap = %q", c.SubmitWaitCap.String())
	return b.String()
}

func tomlList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	parts := make([]string, len(v))
	for i, s := range v {
		parts[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// redactDSN keeps a DSN legible while removing the password, which is the part
// that matters and the part someone reading the output does not need.
//
// The search is bounded to the authority section. A "@" in a query parameter is
// not a credential separator, and taking the last one in the whole string would
// mangle the DSN into something unrecognisable — the opposite of legible.
func redactDSN(dsn string) string {
	scheme := strings.Index(dsn, "://")
	if scheme < 0 {
		return dsn
	}
	rest := dsn[scheme+3:]
	end := len(rest)
	for _, sep := range []byte{'/', '?', '#'} {
		if i := strings.IndexByte(rest, sep); i >= 0 && i < end {
			end = i
		}
	}
	// Last "@" *within* the authority: a password may legitimately contain one.
	at := strings.LastIndexByte(rest[:end], '@')
	if at < 0 {
		return dsn
	}
	creds := rest[:at]
	colon := strings.IndexByte(creds, ':')
	if colon < 0 {
		return dsn // a user with no password: nothing to hide
	}
	return dsn[:scheme+3] + creds[:colon] + ":***" + rest[at:]
}

// ExampleFile is a documented starting point, written by `config init`.
func ExampleFile() string {
	return strings.TrimLeft(`
# VariantHub web service configuration.
#
# Every setting has an environment-variable equivalent that overrides what is
# here, so a container can change one value without rewriting the file. Run
# "varianthub-web config" to print what a given deployment actually resolved to.

[server]
  addr = ":8080"                                    # VHW_ADDR
  # Browser origins allowed to call the API. Leave empty for a same-origin
  # deployment, where advertising CORS would only widen the surface.
  cors_origins = []                                 # VHW_CORS_ORIGINS
  trusted_proxies = ["127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]

[database]
  # Required. Usually left to the environment in a container, where it is
  # assembled from a secret.
  url = "postgres://varianthub:CHANGEME@localhost:5432/varianthub?sslmode=disable"

[auth]
  # Let callers with no account use the annotation flow. Off by default:
  # opening an instance up should be a decision someone made.
  allow_anonymous = false                           # VHW_ALLOW_ANONYMOUS

# Institutional sign-in. All three of client_id, client_secret and redirect_url
# must be set to enable it; an incomplete set leaves sign-in password-only.
[auth.cilogon]
  client_id = ""                                    # VHW_CILOGON_CLIENT_ID
  client_secret = ""                                # VHW_CILOGON_CLIENT_SECRET
  redirect_url = ""                                 # https://host/auth/cilogon/callback
  # Email domains whose verified holders get an account on first sign-in.
  # Empty means invite-only: an administrator creates the account and the first
  # sign-in claims it. CILogon federates thousands of institutions, so
  # authenticating there is not by itself a reason to have an account here.
  auto_provision_domains = []                       # VHW_CILOGON_AUTO_PROVISION_DOMAINS

[worker]
  count = 2                                         # VHW_WORKERS
  varhub_bin = "varhub"                             # VHW_VARHUB_BIN
  # Fixed VARHUB_HOME. Empty means materialize one per job from the catalog,
  # which is what a deployment normally wants.
  varhub_home = ""                                  # VHW_VARHUB_HOME
  data_dir = "/var/lib/varianthub/data"             # VHW_DATA_DIR
  cache_dir = "/var/lib/varianthub/cache"           # VHW_CACHE_DIR
  job_timeout = "1h"                                # VHW_JOB_TIMEOUT
  job_ttl = "24h"                                   # VHW_JOB_TTL

# Where downloaded source data goes. Filesystem entries come first and the first
# of those is the default target. Both take "name=<target>" entries.
[storage]
  paths = ["default=/var/lib/varianthub/sources"]    # VHW_STORAGE_PATHS
  s3 = []                                            # VHW_STORAGE_S3

[limits]
  # The submit rate applies to anonymous callers only; an account is
  # accountable, and throttling a signed-in bulk load would make it throttle
  # itself. The concurrency cap below applies to everyone.
  rate_per_min = 30                                 # VHW_RATE_PER_MIN
  rate_burst = 10                                   # VHW_RATE_BURST
  max_jobs_per_ip = 2                               # VHW_MAX_JOBS_PER_IP
  max_upload_bytes = 67108864                       # VHW_MAX_UPLOAD_BYTES
  submit_wait_cap = "10s"                           # VHW_SUBMIT_WAIT_CAP
`, "\n")
}

// ConfigPath reports which file Load() would read, for diagnostics.
func ConfigPath() string {
	p, err := findConfigFile()
	if err != nil {
		return ""
	}
	return p
}

// AbsConfigPath is ConfigPath made absolute where possible, so a relative
// search-path hit is unambiguous in a log line.
func AbsConfigPath() string {
	p := ConfigPath()
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
