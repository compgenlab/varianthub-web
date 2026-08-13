package config

import (
	"fmt"
	"os"
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
		Addr string `toml:"addr"`
		// PublicURL is where this installation answers from. The CORS origin
		// and the CILogon callback are derived from it when they are not set.
		PublicURL      string   `toml:"public_url"`
		CORSOrigins    []string `toml:"cors_origins"`
		TrustedProxies []string `toml:"trusted_proxies"`
	} `toml:"server"`

	Database struct {
		URL string `toml:"url"`
	} `toml:"database"`

	Cache struct {
		Enabled    *bool  `toml:"enabled"`
		MaxEntries *int64 `toml:"max_entries"`
		MaxAge     string `toml:"max_age"`
	} `toml:"cache"`

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
		Count           *int   `toml:"count"`
		Slots           *int   `toml:"slots"`
		DownloadWeight  *int   `toml:"download_weight"`
		VarhubBin       string `toml:"varhub_bin"`
		VarhubHome      string `toml:"varhub_home"`
		DataDir         string `toml:"data_dir"`
		CacheDir        string `toml:"cache_dir"`
		JobStorage      string `toml:"job_storage"`
		JobTimeout      string `toml:"job_timeout"`
		DownloadTimeout string `toml:"download_timeout"`
		JobTTL          string `toml:"job_ttl"`
		// A pointer so "unset" is distinct from "false": an operator turning the
		// cache off in the file must not be silently re-enabled by the default.
		NoCache *bool `toml:"no_cache"`
	} `toml:"worker"`

	// References maps an assembly to a FASTA path. A bare table so an assembly
	// name is the key: [references]\n  GRCh38 = "/mnt/ref/GRCh38.fa"
	References map[string]string `toml:"references"`

	Storage struct {
		Paths []string `toml:"paths"`
		S3    []string `toml:"s3"`
	} `toml:"storage"`

	// S3 sites, each with its own endpoint and credentials.
	//
	// [storage] s3 names locations and takes credentials from the process
	// environment, which is one set for everything. That cannot express two
	// targets with different credentials — a gateway on the cluster and a
	// bucket at a provider, say — so a site carries its own here.
	S3Sites []S3Site `toml:"s3"`

	Limits struct {
		RatePerMin   *int `toml:"rate_per_min"`
		RateBurst    *int `toml:"rate_burst"`
		MaxJobsPerIP *int `toml:"max_jobs_per_ip"`

		// Per-tier service limits. Concurrent running jobs, and submissions per
		// hour; 0 is unbounded. An administrator can override each from the
		// settings form, so these describe a fresh installation rather than
		// being the last word.
		AnonConcurrent     *int `toml:"anon_concurrent"`
		AnonPerHour        *int `toml:"anon_per_hour"`
		StandardConcurrent *int `toml:"standard_concurrent"`
		StandardPerHour    *int `toml:"standard_per_hour"`
		ElevatedConcurrent *int `toml:"elevated_concurrent"`
		ElevatedPerHour    *int `toml:"elevated_per_hour"`

		// The most variants one submission may carry, per tier; 0 is unbounded.
		AnonMaxVariants     *int `toml:"anon_max_variants"`
		StandardMaxVariants *int `toml:"standard_max_variants"`
		ElevatedMaxVariants *int `toml:"elevated_max_variants"`
		// Variants per chunk of a split VCF. Sized by per-chunk fixed cost, not
		// by fairness — see catalog.Site.VCFChunkSize.
		VCFChunkSize   *int   `toml:"vcf_chunk_size"`
		MaxUploadBytes *int64 `toml:"max_upload_bytes"`
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
	setStr(&c.PublicURL, f.Server.PublicURL)
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
	setInt(&c.JobSlots, f.Worker.Slots)
	setInt(&c.DownloadWeight, f.Worker.DownloadWeight)
	setStr(&c.VarhubBin, f.Worker.VarhubBin)
	setStr(&c.VarhubHome, f.Worker.VarhubHome)
	setStr(&c.DataDir, f.Worker.DataDir)
	setStr(&c.CacheDir, f.Worker.CacheDir)
	setStr(&c.JobStorage, f.Worker.JobStorage)
	if f.Cache.Enabled != nil {
		c.CacheEnabled = *f.Cache.Enabled
	}
	if f.Cache.MaxEntries != nil {
		c.CacheMaxEntries = *f.Cache.MaxEntries
	}
	if f.Cache.MaxAge != "" {
		c.CacheMaxAge = f.Cache.MaxAge
	}
	if f.Worker.NoCache != nil {
		c.NoCache = *f.Worker.NoCache
	}
	if err := setDur(&c.JobTimeout, f.Worker.JobTimeout, path, "worker.job_timeout"); err != nil {
		return hasSecret, err
	}
	if err := setDur(&c.DownloadTimeout, f.Worker.DownloadTimeout, path, "worker.download_timeout"); err != nil {
		return hasSecret, err
	}
	if err := setDur(&c.JobTTL, f.Worker.JobTTL, path, "worker.job_ttl"); err != nil {
		return hasSecret, err
	}

	if len(f.References) > 0 {
		c.References = f.References
	}
	setList(&c.StoragePaths, f.Storage.Paths)
	setList(&c.StorageS3, f.Storage.S3)
	if len(f.S3Sites) > 0 {
		c.S3Sites = f.S3Sites
	}

	setInt(&c.RatePerMin, f.Limits.RatePerMin)
	setInt(&c.RateBurst, f.Limits.RateBurst)
	setInt(&c.MaxJobsPerIP, f.Limits.MaxJobsPerIP)
	setInt(&c.AnonConcurrent, f.Limits.AnonConcurrent)
	setInt(&c.AnonPerHour, f.Limits.AnonPerHour)
	setInt(&c.StandardConcurrent, f.Limits.StandardConcurrent)
	setInt(&c.StandardPerHour, f.Limits.StandardPerHour)
	setInt(&c.ElevatedConcurrent, f.Limits.ElevatedConcurrent)
	setInt(&c.ElevatedPerHour, f.Limits.ElevatedPerHour)
	setInt(&c.AnonMaxVariants, f.Limits.AnonMaxVariants)
	setInt(&c.StandardMaxVariants, f.Limits.StandardMaxVariants)
	setInt(&c.ElevatedMaxVariants, f.Limits.ElevatedMaxVariants)
	setInt(&c.VCFChunkSize, f.Limits.VCFChunkSize)
	if f.Limits.MaxUploadBytes != nil {
		c.MaxUploadBytes = *f.Limits.MaxUploadBytes
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

// redactDSN removes the password from a DSN.
//
// Used to decide whether a config file carries a secret — a DSN counts only
// when it has a password in it, since one relying on a peer or IAM credential
// has nothing to protect. Comparing the redacted form against the original is
// the whole test.
//
// The search is bounded to the authority section: a "@" in a query parameter is
// not a credential separator, and taking the last one in the whole string would
// find a separator that is not there.
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

// S3Site is one object-storage target: where it is, and what to present to it.
//
// Credentials are optional. Without them the AWS SDK's own chain applies —
// environment, shared config, instance role — which is what a deployment using
// a role rather than keys wants, and is the only form that does not put a
// secret in a file.
type S3Site struct {
	// Name is the storage location id this site provides, matching the name
	// used in a source's cache_dir.
	Name string `toml:"name"`
	// URI is the bucket and prefix, e.g. "s3://varianthub-sources/annotations".
	URI string `toml:"uri"`
	// Endpoint is an S3-compatible gateway. Empty means AWS itself.
	Endpoint string `toml:"endpoint"`
	Region   string `toml:"region"`
	// AccessKey and SecretKey are presented when both are set.
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	// Default marks the location downloads target when none is named.
	Default bool `toml:"default"`
}
