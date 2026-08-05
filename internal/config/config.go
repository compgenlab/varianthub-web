// Package config is the service's configuration: defaults, then an optional
// TOML file, then the environment, each overriding the one before.
//
// The file is the readable record of how a deployment is set up — one place to
// look rather than a dozen variables spread across a compose file and a shell
// profile. The environment stays authoritative on top of it so a container can
// override a single value without templating a whole file, and so a deployment
// that has no file keeps working exactly as before.
//
// Note this is the *service's* config. The annotation catalog — sources,
// snapshots, and the varhub config.toml the worker materializes per job — is a
// separate concern living in Postgres.
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved service configuration.
type Config struct {
	Addr        string // listen address
	DatabaseURL string // Postgres DSN

	// AllowAnonymous lets a caller with no credential use the annotation flow.
	// A public instance wants this on; a private one wants every request to
	// carry an identity. Off by default: opening an instance up should be a
	// decision someone made, not one they inherited.
	AllowAnonymous bool

	// CILogon (OIDC). All three are required to enable it; an incomplete set
	// leaves sign-in password-only rather than half-configured.
	CILogonClientID     string
	CILogonClientSecret string
	CILogonRedirectURL  string
	// CILogonAutoProvision are email domains whose verified holders get an
	// account on first sign-in. Empty — the default — means invite-only: an
	// administrator creates the account and the first sign-in claims it.
	// CILogon federates thousands of institutions, so authenticating there is
	// not by itself a reason to have an account here.
	CILogonAutoProvision []string

	Workers int // worker pool size
	// JobSlots is the pool's capacity measured in job weight rather than job
	// count. Defaults to Workers, which makes a 1-weight job behave exactly as
	// before.
	JobSlots int
	// DownloadWeight is how many slots a provisioning job holds. 2 against the
	// default 2 slots makes downloads exclusive of each other, which is the
	// point: two large downloads on one machine finish later than one after the
	// other, and gain nothing by overlapping.
	//
	// A deployment that wants annotation to continue during a long provisioning
	// run raises JobSlots above this rather than lowering it — the download
	// should still hold what it actually costs.
	DownloadWeight int
	VarhubBin      string // path to the varhub CLI
	VarhubHome     string // fixed VARHUB_HOME; empty = materialize per job from the catalog
	DataDir        string // shared, persistent: downloaded source files
	CacheDir       string // shared, persistent: built indexes and the annotation cache
	// StoragePaths are filesystem download targets declared by the deployment, as
	// "name=/abs/path" entries. The first is the default. They are reconciled into
	// the catalog at startup so the config file stays authoritative for them.
	StoragePaths []string

	// StorageS3 are object-store download targets declared by the deployment,
	// as "name=s3://bucket/prefix". Separate from StoragePaths because the two
	// are validated differently and because a deployment usually has one of
	// each, not a mixed list.
	StorageS3 []string
	// References maps an assembly to a reference FASTA on the worker's
	// filesystem, e.g. {"GRCh38": "/mnt/ref/GRCh38.fa"}.
	//
	// Declared by the deployment rather than in the catalog, for the same reason
	// storage paths are: a path only means something if the worker can open it,
	// and a tool step binds the FASTA's directory into a container.
	//
	// Assembly names are matched exactly and deliberately not normalized —
	// "GRCh38" and "hg38" are different keys. A false mismatch is a loud error
	// fixed by editing one line; a false match would annotate against the wrong
	// coordinates and say nothing.
	References map[string]string
	JobTimeout time.Duration // per-job wall clock
	// DownloadTimeout bounds a provisioning job. Longer than JobTimeout by
	// default: a tool's one-time install fetches tens of gigabytes, and being
	// killed partway leaves a half-populated data directory that the next
	// attempt has to redo from the start.
	DownloadTimeout time.Duration
	JobTTL          time.Duration // terminal jobs GC'd after this
	SubmitWaitCap   time.Duration // ceiling on ?wait=
	MaxUploadBytes  int64         // cap on a POST /annotate/vcf body

	RatePerMin   int      // per-IP submit rate
	RateBurst    int      // per-IP burst
	MaxJobsPerIP int      // per-IP concurrent running jobs
	TrustedProxy []string // CIDRs whose X-Forwarded-For is believed
	CORSOrigins  []string // allowed browser origins for the SPA
	Version      string   // build stamp, surfaced at /version
}

// Defaults returns the configuration with nothing configured.
//
// Kept as its own function so the defaults are stated once and are what both
// Load and the example file describe — the previous shape buried them as the
// second argument of twenty env lookups, where they could drift from the docs
// without anything noticing.
func Defaults() *Config {
	return &Config{
		Addr:            ":8080",
		Workers:         2,
		JobSlots:        0, // 0 = follow Workers
		DownloadWeight:  2,
		VarhubBin:       "varhub",
		DataDir:         "/var/lib/varianthub/data",
		CacheDir:        "/var/lib/varianthub/cache",
		StoragePaths:    []string{"default=/var/lib/varianthub/sources"},
		JobTimeout:      time.Hour,
		DownloadTimeout: 12 * time.Hour,
		JobTTL:          24 * time.Hour,
		SubmitWaitCap:   10 * time.Second,
		MaxUploadBytes:  64 << 20,
		RatePerMin:      30,
		RateBurst:       10,
		MaxJobsPerIP:    2,
		References:      map[string]string{},
		TrustedProxy: []string{
			"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		},
	}
}

// Load resolves the configuration: defaults, then the file, then the
// environment.
func Load() (*Config, error) {
	c := Defaults()

	path, err := findConfigFile()
	if err != nil {
		return nil, err
	}
	if path != "" {
		fileHasSecret, err := applyFile(c, path)
		if err != nil {
			return nil, err
		}
		log.Printf("config: loaded %s", path)
		warnIfWorldReadable(path, fileHasSecret, log.Printf)
	}
	applyEnv(c)

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("no database configured: set database.url in %s, or VHW_DATABASE_URL",
			configHint(path))
	}
	// Removed in favour of accounts. Warned about rather than ignored: a
	// deployment still setting these believes they do something, and the shape
	// of "the API is protected" has changed under it.
	for _, k := range []string{"VHW_MASTER_KEY", "VHW_REQUIRE_TOKEN"} {
		if os.Getenv(k) != "" {
			log.Printf("config: %s is set but no longer used — /api/v1 now requires "+
				"an account, and VHW_ALLOW_ANONYMOUS is the only opt-out", k)
		}
	}
	if c.JobSlots <= 0 {
		c.JobSlots = c.Workers
	}
	return c, nil
}

// configHint names the file in an error, or suggests where one would go.
func configHint(path string) string {
	if path != "" {
		return path
	}
	return "a config file (" + SearchPaths[0] + ")"
}

// applyEnv overlays environment variables, which win over the file.
//
// Every setting is overridable, without exception: a value that could only be
// set in the file would be one a container could not change, and finding that
// out during an incident is not the moment.
func applyEnv(c *Config) {
	envStr("VHW_ADDR", &c.Addr)
	envStr("VHW_DATABASE_URL", &c.DatabaseURL)
	envBoolInto("VHW_ALLOW_ANONYMOUS", &c.AllowAnonymous)

	envStr("VHW_CILOGON_CLIENT_ID", &c.CILogonClientID)
	envStr("VHW_CILOGON_CLIENT_SECRET", &c.CILogonClientSecret)
	envStr("VHW_CILOGON_REDIRECT_URL", &c.CILogonRedirectURL)
	envListInto("VHW_CILOGON_AUTO_PROVISION_DOMAINS", &c.CILogonAutoProvision)

	envIntInto("VHW_WORKERS", &c.Workers)
	envIntInto("VHW_JOB_SLOTS", &c.JobSlots)
	envIntInto("VHW_DOWNLOAD_WEIGHT", &c.DownloadWeight)
	envStr("VHW_VARHUB_BIN", &c.VarhubBin)
	envStr("VHW_VARHUB_HOME", &c.VarhubHome)
	envStr("VHW_DATA_DIR", &c.DataDir)
	envStr("VHW_CACHE_DIR", &c.CacheDir)
	envDurInto("VHW_JOB_TIMEOUT", &c.JobTimeout)
	envDurInto("VHW_DOWNLOAD_TIMEOUT", &c.DownloadTimeout)
	envDurInto("VHW_JOB_TTL", &c.JobTTL)

	envListInto("VHW_STORAGE_PATHS", &c.StoragePaths)
	envListInto("VHW_STORAGE_S3", &c.StorageS3)
	if v, ok := lookup("VHW_REFERENCES"); ok {
		// "GRCh38=/mnt/ref/GRCh38.fa,GRCh37=/mnt/ref/hs37d5.fa" — the same
		// name=value shape as the storage variables.
		refs := map[string]string{}
		for _, entry := range strings.Split(v, ",") {
			name, path, found := strings.Cut(strings.TrimSpace(entry), "=")
			if !found {
				continue
			}
			if name, path = strings.TrimSpace(name), strings.TrimSpace(path); name != "" && path != "" {
				refs[name] = path
			}
		}
		c.References = refs
	}

	envIntInto("VHW_RATE_PER_MIN", &c.RatePerMin)
	envIntInto("VHW_RATE_BURST", &c.RateBurst)
	envIntInto("VHW_MAX_JOBS_PER_IP", &c.MaxJobsPerIP)
	envDurInto("VHW_SUBMIT_WAIT_CAP", &c.SubmitWaitCap)
	if v, ok := lookup("VHW_MAX_UPLOAD_BYTES"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.MaxUploadBytes = n
		}
	}
	envListInto("VHW_TRUSTED_PROXIES", &c.TrustedProxy)
	envListInto("VHW_CORS_ORIGINS", &c.CORSOrigins)
}

// lookup reads an environment variable, treating empty as unset. An empty
// string cannot mean "override the file with nothing" — that is what the file
// itself is for, and the ambiguity would make an accidentally-blank variable
// silently erase a configured value.
func lookup(k string) (string, bool) {
	v, ok := os.LookupEnv(k)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

func envStr(k string, dst *string) {
	if v, ok := lookup(k); ok {
		*dst = v
	}
}

func envIntInto(k string, dst *int) {
	if v, ok := lookup(k); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func envBoolInto(k string, dst *bool) {
	if v, ok := lookup(k); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func envDurInto(k string, dst *time.Duration) {
	if v, ok := lookup(k); ok {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}

func envListInto(k string, dst *[]string) {
	v, ok := lookup(k)
	if !ok {
		return
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	*dst = out
}

// StorageLocations parses the deployment's declared download targets.
//
// storage.paths (VHW_STORAGE_PATHS) holds filesystem targets as
// "name=/abs/path"; a bare path is accepted and named after its last element,
// so the common single-volume case needs no ceremony. storage.s3
// (VHW_STORAGE_S3) holds object-store targets as "name=s3://bucket/prefix".
//
// Filesystem entries come first and the first of those is the default download
// target, because a deployment that has both usually wants the local volume as
// the default and the bucket as an explicit choice.
func (c *Config) StorageLocations() ([]struct {
	ID, Name, Path, Kind string
	Default              bool
}, error) {
	type loc = struct {
		ID, Name, Path, Kind string
		Default              bool
	}
	var out []loc
	seen := map[string]bool{}

	add := func(raw, envName, kind string, isDefault bool, check func(string) error) error {
		name, p, ok := strings.Cut(raw, "=")
		if !ok {
			p = name
			name = filepath.Base(strings.TrimRight(p, "/"))
		}
		name = strings.TrimSpace(name)
		p = strings.TrimSpace(p)
		if name == "" || p == "" {
			return fmt.Errorf("%s entry %q: want name=<target>", envName, raw)
		}
		if err := check(p); err != nil {
			return fmt.Errorf("%s entry %q: %w", envName, raw, err)
		}
		id := "cfg-" + strings.ToLower(name)
		if seen[id] {
			return fmt.Errorf("%s declares %q twice", envName, name)
		}
		seen[id] = true
		out = append(out, loc{ID: id, Name: name, Path: p, Kind: kind, Default: isDefault})
		return nil
	}

	for i, raw := range c.StoragePaths {
		if err := add(raw, "storage.paths", "path", i == 0, func(p string) error {
			if !filepath.IsAbs(p) {
				return fmt.Errorf("path must be absolute")
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for i, raw := range c.StorageS3 {
		if err := add(raw, "storage.s3", "s3", len(c.StoragePaths) == 0 && i == 0, func(p string) error {
			if !strings.HasPrefix(p, "s3://") {
				return fmt.Errorf("target must be an s3:// URI")
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}
