// Package config is the service's environment-driven configuration.
//
// Everything is read from the environment rather than a file: this runs in
// containers, where env vars and mounted secrets are the native mechanism. Note
// this is the *service's* config — the annotation catalog (sources, snapshots)
// is a separate concern that Chunk 2 moves into Postgres.
package config

import (
	"fmt"
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
	MasterKey   string // HMAC key signing API tokens

	RequireToken bool // bearer auth on /api/v1

	Workers    int    // worker pool size
	VarhubBin  string // path to the varhub CLI
	VarhubHome string // fixed VARHUB_HOME; empty = materialize per job from the catalog
	DataDir    string // shared, persistent: downloaded source files
	CacheDir   string // shared, persistent: built indexes and the annotation cache
	// StoragePaths are filesystem download targets declared by the deployment, as
	// "name=/abs/path" entries. The first is the default. They are reconciled into
	// the catalog at startup so the config file stays authoritative for them.
	StoragePaths []string

	// StorageS3 are object-store download targets declared by the deployment,
	// as "name=s3://bucket/prefix". Separate from StoragePaths because the two
	// are validated differently and because a deployment usually has one of
	// each, not a mixed list.
	StorageS3      []string
	JobTimeout     time.Duration // per-job wall clock
	JobTTL         time.Duration // terminal jobs GC'd after this
	SubmitWaitCap  time.Duration // ceiling on ?wait=
	MaxUploadBytes int64         // cap on a POST /annotate/vcf body

	RatePerMin   int      // per-IP submit rate
	RateBurst    int      // per-IP burst
	MaxJobsPerIP int      // per-IP concurrent running jobs
	TrustedProxy []string // CIDRs whose X-Forwarded-For is believed
	CORSOrigins  []string // allowed browser origins for the SPA
	Version      string   // build stamp, surfaced at /version
}

// Load reads the configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Addr:         env("VHW_ADDR", ":8080"),
		DatabaseURL:  os.Getenv("VHW_DATABASE_URL"),
		MasterKey:    os.Getenv("VHW_MASTER_KEY"),
		RequireToken: envBool("VHW_REQUIRE_TOKEN", true),
		Workers:      envInt("VHW_WORKERS", 2),
		VarhubBin:    env("VHW_VARHUB_BIN", "varhub"),
		VarhubHome:   os.Getenv("VHW_VARHUB_HOME"),
		DataDir:      env("VHW_DATA_DIR", "/var/lib/varianthub/data"),
		CacheDir:     env("VHW_CACHE_DIR", "/var/lib/varianthub/cache"),
		StorageS3:    envList("VHW_STORAGE_S3", nil),
		StoragePaths: envList("VHW_STORAGE_PATHS",
			[]string{"default=/var/lib/varianthub/sources"}),
		JobTimeout:     envDur("VHW_JOB_TIMEOUT", time.Hour),
		JobTTL:         envDur("VHW_JOB_TTL", 24*time.Hour),
		SubmitWaitCap:  envDur("VHW_SUBMIT_WAIT_CAP", 10*time.Second),
		MaxUploadBytes: int64(envInt("VHW_MAX_UPLOAD_BYTES", 64<<20)),
		RatePerMin:     envInt("VHW_RATE_PER_MIN", 30),
		RateBurst:      envInt("VHW_RATE_BURST", 10),
		MaxJobsPerIP:   envInt("VHW_MAX_JOBS_PER_IP", 2),
		TrustedProxy: envList("VHW_TRUSTED_PROXIES",
			[]string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}),
		CORSOrigins: envList("VHW_CORS_ORIGINS", nil),
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("VHW_DATABASE_URL is required")
	}
	// An empty master key with auth on means HMAC over an empty key, which anyone
	// can forge. Refuse rather than appear secured.
	if c.RequireToken && c.MasterKey == "" {
		return nil, fmt.Errorf("VHW_MASTER_KEY is required (or set VHW_REQUIRE_TOKEN=false for an open API)")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envList(k string, def []string) []string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// StorageLocations parses the deployment's declared download targets.
//
// VHW_STORAGE_PATHS holds filesystem targets as "name=/abs/path"; a bare path
// is accepted and named after its last element, so the common single-volume
// case needs no ceremony. VHW_STORAGE_S3 holds object-store targets as
// "name=s3://bucket/prefix". Both are comma-separated.
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
		if err := add(raw, "VHW_STORAGE_PATHS", "path", i == 0, func(p string) error {
			if !filepath.IsAbs(p) {
				return fmt.Errorf("path must be absolute")
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for i, raw := range c.StorageS3 {
		if err := add(raw, "VHW_STORAGE_S3", "s3", len(c.StoragePaths) == 0 && i == 0, func(p string) error {
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
