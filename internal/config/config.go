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

// DefaultVCFChunkSize is how many variants one chunk of a split VCF carries
// when nothing says otherwise.
//
// Defined here rather than in catalog because catalog imports this package, and
// the value has to be one number: config supplies the default and catalog's
// stored override falls back to it, so two definitions would let a cleared
// setting revert to something other than the configured default.
//
// Not the interactive submission cap, though both started at 10,000. That one
// bounds what a caller may ask for; this bounds how much work one process does,
// and it is set by fixed per-chunk cost — a claim, a stage from storage, a
// varhub start, a snapshot load, then an upload and a merge. Chromosome 22 of a
// WGS cohort is 2.6M variants: 260 chunks at 10,000, 26 at this.
const DefaultVCFChunkSize = 100_000

// Config is the resolved service configuration.
type Config struct {
	Addr string // listen address, e.g. ":8080" or "10.0.0.5:8080"
	// PublicURL is where this installation answers from, e.g.
	// "https://varianthub.compgenlab.org".
	//
	// One setting because the domain was otherwise written three times — the
	// CORS origin, the CILogon callback, and the address a generated API client
	// points at — and three copies of a hostname is three chances to move one
	// and forget another. Each still has its own override for the cases that
	// genuinely differ; this is what they fall back to.
	PublicURL   string
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
	// JobStorage is where a job's input and result files live: an "s3://bucket/prefix"
	// or an absolute filesystem path. Both the API and the worker write here, so
	// on a multi-node deployment it must be a bucket or a shared mount — a
	// pod-local path means the worker cannot read what the API accepted.
	//
	// Deliberately not one of the catalog's storage locations. Those hold source
	// data: an administrator browses them, they are reconciled from config at
	// startup, and their contents are the installation's asset. This holds
	// per-job scratch that is deleted when the job expires, and mixing the two
	// would put a week of transient uploads into the storage browser and into
	// every usage total.
	JobStorage string
	// StoragePaths are filesystem download targets declared by the deployment, as
	// "name=/abs/path" entries. The first is the default. They are reconciled into
	// the catalog at startup so the config file stays authoritative for them.
	StoragePaths []string

	// StorageS3 are object-store download targets declared by the deployment,
	// as "name=s3://bucket/prefix". Separate from StoragePaths because the two
	// are validated differently and because a deployment usually has one of
	// each, not a mixed list.
	StorageS3 []string

	// S3Sites are object-storage targets that carry their own endpoint and
	// credentials, declared as [[s3]] blocks. A site is a storage location too,
	// so it appears in StorageLocations alongside storage.paths and storage.s3.
	S3Sites []S3Site
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
	// NoCache bypasses varhub's annotation cache, computing every value fresh.
	// For diagnosis: a cached value is indistinguishable from a fresh one in the
	// result, so this is what separates "asked and got nothing" from "replaying
	// an older, emptier answer" — including one cached before a source was
	// installed. Off by default; the cache is what makes a repeated query cheap.
	NoCache bool

	// The shared annotation cache, as this deployment is configured. These are
	// defaults: an administrator can override each from the settings form, and
	// the stored value wins (see catalog.Site). The file remains the description
	// of a fresh deployment.
	//
	// Distinct from NoCache, which is a diagnostic that computes fresh values
	// while leaving the cache configured. CacheEnabled false means jobs are given
	// no cache at all.
	CacheEnabled bool
	// CacheMaxEntries caps cached (variant, source) units; 0 is unbounded.
	CacheMaxEntries int64
	// CacheMaxAge discards entries unused for longer, as a duration string.
	CacheMaxAge string

	// Service limits per tier: concurrent running jobs, and submissions per
	// hour. 0 is unbounded. Defaults here, overridable per installation from the
	// settings form — so a deployment is described by its file and tuned without
	// one.
	//
	// Per hour rather than per minute, which is the unit the per-IP submit rate
	// uses: "one job every five minutes" is what an anonymous visitor should
	// get, and per-minute integers cannot express it.
	AnonConcurrent     int
	AnonPerHour        int
	StandardConcurrent int
	StandardPerHour    int
	ElevatedConcurrent int
	ElevatedPerHour    int

	// The most variants one submission may carry, per tier; 0 is unbounded.
	AnonMaxVariants     int
	StandardMaxVariants int
	ElevatedMaxVariants int
	// VCFChunkSize is variants per chunk of a split VCF. A different question
	// from the caps above and sized differently — see catalog.Site.VCFChunkSize.
	VCFChunkSize int

	// JobTimeout bounds a single annotation job's wall clock.
	//
	// Twelve hours by default, which is a ceiling rather than an expectation:
	// an ordinary job finishes in seconds, and this is sized for the largest one
	// the limits allow — a whole chromosome of a WGS cohort against several
	// expensive sources. An hour used to be the figure, and it killed exactly
	// the submissions that took longest to prepare and cost the most to redo.
	//
	// The queue's protection against a job occupying a worker forever is the
	// weight budget and the concurrency cap, not this. Cutting a job off at an
	// arbitrary hour does not free capacity, it wastes the capacity already
	// spent.
	JobTimeout time.Duration
	// DownloadTimeout bounds a provisioning job.
	//
	// Longer than JobTimeout, and it has to stay longer: the two are sized by
	// different things. An annotation is bounded by how much work one
	// submission may carry; a tool's one-time install is bounded by how long it
	// takes to fetch tens of gigabytes down a single TCP stream. A measured VEP
	// install took about 17 hours, so the 12h this used to default to would have
	// killed it five hours from done — and being killed partway is worse than
	// being slow, because it leaves a half-populated data directory with no
	// sentinel and the next attempt starts from nothing.
	DownloadTimeout time.Duration
	JobTTL          time.Duration // terminal jobs GC'd after this
	// MaxUploadBytes caps a POST /annotate/vcf body.
	//
	// Held at 64 MB deliberately, below the size the limits will eventually
	// allow. The upload is currently read into memory in one piece and stored
	// as a Postgres BYTEA, so this figure is bounded by what the API process
	// can hold rather than by what a caller should be able to send — and a
	// whole chromosome of a WGS cohort is around 150 MB compressed. Raising it
	// before the upload streams to object storage would turn a rejected request
	// into an OOM.
	//
	// It is a backstop in any case, not the limit that matters: the variant
	// count is. A byte cap alone cannot tell a large file of few richly-
	// annotated variants from a small one holding millions.
	MaxUploadBytes int64

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
		Addr:           ":8080",
		Workers:        2,
		JobSlots:       0, // 0 = follow Workers
		DownloadWeight: 2,
		VarhubBin:      "varhub",
		DataDir:        "/var/lib/varianthub/data",
		CacheDir:       "/var/lib/varianthub/cache",
		// A filesystem default, not a bucket: a single-node installation should
		// come up without object storage configured. It sits beside the other
		// var directories rather than inside DataDir, because that one holds
		// source data an administrator keeps and this holds job scratch that is
		// deleted on a timer — the two should not be one `rm -rf` apart.
		JobStorage:      "/var/lib/varianthub/jobs",
		StoragePaths:    []string{"default=/var/lib/varianthub/sources"},
		JobTimeout:      12 * time.Hour,
		DownloadTimeout: 48 * time.Hour,
		// Results are the thing a user comes back for, and a day is shorter
		// than the gap between someone running an annotation and being asked
		// about it. A week covers that without storing every result an
		// installation ever produced.
		//
		// The cost is real and worth knowing: a result is stored twice, once as
		// the JSON blob that backs export and once as rows that back paging and
		// sorting, so this multiplies the larger of those by seven.
		JobTTL:         7 * 24 * time.Hour,
		MaxUploadBytes: 64 << 20,
		RatePerMin:     30,
		RateBurst:      10,
		MaxJobsPerIP:   2,

		// The cache on, with a budget. It was absent here, so the zero value
		// applied and a fresh installation cached nothing at all — while both
		// example files said enabled = true under a heading promising to
		// describe the defaults. One of the two had to move, and an annotation
		// service whose cache is off until somebody notices is the wrong one.
		//
		// 50M (variant, source) units is roughly ten sources over a whole
		// genome. It is the disk a deployment is willing to spend, not a tuning
		// parameter: being wrong costs recomputation, never correctness.
		CacheEnabled:    true,
		CacheMaxEntries: 50_000_000,
		CacheMaxAge:     "2160h",

		// An anonymous visitor gets one job at a time and one every five
		// minutes: enough to try the service, not enough to occupy it. An
		// account gets one at a time and one a minute, which is what a public
		// deployment can promise everybody at once.
		AnonConcurrent:     1,
		AnonPerHour:        12,
		StandardConcurrent: 1,
		StandardPerHour:    60,
		ElevatedConcurrent: 5,
		ElevatedPerHour:    600,

		// Ten thousand for anyone who has not been given more. It is what a
		// person pastes into a box, and past it the VCF path is the right tool.
		// Elevated is uncapped: that tier exists for cohort work, where a whole
		// chromosome in one submission is the point.
		AnonMaxVariants:     10_000,
		StandardMaxVariants: 10_000,
		ElevatedMaxVariants: 0,
		VCFChunkSize:        DefaultVCFChunkSize,

		References: map[string]string{},
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
	if err := c.checkJobStorage(configHint(path)); err != nil {
		return nil, err
	}
	c.applyPublicURL()
	return c, nil
}

// checkJobStorage refuses a job storage location that cannot be shared.
//
// The API writes a job's input and the worker reads it, so they must see the
// same bytes — a bucket, or a directory both processes have mounted. Whether it
// is object storage or a filesystem does not matter; whether it is *shared*
// does, and only one of the two ways to get that wrong is visible from in here.
//
// A relative path is the visible one, and it is always wrong: it resolves
// against each process's working directory, so the API and the worker would
// each get their own. That fails at the first upload with "no such file",
// pointing at the worker rather than at the configuration.
//
// The other way — an absolute path that is pod-local rather than a shared mount
// — cannot be detected from a single process, since it looks identical to a
// correct shared mount. The deployment files are where that is settled, and the
// error below says so rather than implying this check is complete.
func (c *Config) checkJobStorage(hint string) error {
	s := strings.TrimSpace(c.JobStorage)
	if s == "" {
		return fmt.Errorf("no job storage configured: set worker.job_storage in %s, "+
			"or VHW_JOB_STORAGE. It holds job inputs and results, and the API and "+
			"the worker must both reach it — an s3:// bucket, or a directory mounted "+
			"into both", hint)
	}
	if strings.HasPrefix(s, "s3://") {
		if strings.TrimPrefix(s, "s3://") == "" {
			return fmt.Errorf("worker.job_storage %q names no bucket", s)
		}
		c.JobStorage = strings.TrimRight(s, "/")
		return nil
	}
	if !filepath.IsAbs(s) {
		return fmt.Errorf("worker.job_storage %q must be absolute or an s3:// URI: a "+
			"relative path resolves against each process's working directory, so the "+
			"API and the worker would not share one", s)
	}
	c.JobStorage = filepath.Clean(s)
	return nil
}

// applyPublicURL fills in what the site's address implies.
//
// Only where nothing was set explicitly: a deployment that names its CORS
// origins or its CILogon callback has a reason to, and this must not overrule
// it. The point is that the ordinary case — one hostname, everything derived —
// needs one setting instead of three that can disagree.
func (c *Config) applyPublicURL() {
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	if c.PublicURL == "" {
		return
	}
	if len(c.CORSOrigins) == 0 {
		// The browser calling this API is the app served from the same origin.
		c.CORSOrigins = []string{c.PublicURL}
	}
	if c.CILogonRedirectURL == "" {
		// The path is fixed by the route this server registers, so deriving it
		// removes a value that can only ever be wrong by being out of step.
		c.CILogonRedirectURL = c.PublicURL + "/auth/cilogon/callback"
	}
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
	envStr("VHW_PUBLIC_URL", &c.PublicURL)
	envStr("VHW_DATABASE_URL", &c.DatabaseURL)
	envBoolInto("VHW_ALLOW_ANONYMOUS", &c.AllowAnonymous)
	envBoolInto("VHW_NO_CACHE", &c.NoCache)
	envBoolInto("VHW_CACHE_ENABLED", &c.CacheEnabled)

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
	envStr("VHW_JOB_STORAGE", &c.JobStorage)
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
	envIntInto("VHW_ANON_CONCURRENT", &c.AnonConcurrent)
	envIntInto("VHW_ANON_PER_HOUR", &c.AnonPerHour)
	envIntInto("VHW_STANDARD_CONCURRENT", &c.StandardConcurrent)
	envIntInto("VHW_STANDARD_PER_HOUR", &c.StandardPerHour)
	envIntInto("VHW_ELEVATED_CONCURRENT", &c.ElevatedConcurrent)
	envIntInto("VHW_ELEVATED_PER_HOUR", &c.ElevatedPerHour)
	envIntInto("VHW_ANON_MAX_VARIANTS", &c.AnonMaxVariants)
	envIntInto("VHW_STANDARD_MAX_VARIANTS", &c.StandardMaxVariants)
	envIntInto("VHW_ELEVATED_MAX_VARIANTS", &c.ElevatedMaxVariants)
	envIntInto("VHW_VCF_CHUNK_SIZE", &c.VCFChunkSize)
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

// envListInto sets a list from a comma-separated variable.
//
// Unlike the scalar helpers this uses LookupEnv directly, so a variable that is
// present and empty means an empty list rather than "not set". A list needs that
// and a string does not: some defaulted lists have no other way to be emptied.
// storage.paths is the case that forced it — there is a built-in
// default=/var/lib/varianthub/sources unless it is set, the config file cannot
// clear it (setList reads an empty TOML list as "not stated", since absent and
// empty are indistinguishable there), and so a deployment that stores sources
// only in an object store had no way to say so. It got a second storage
// location it never asked for, also flagged default, and which of the two a
// download targeted was then a coin toss between a bucket and a path inside a
// container.
//
// The risk the scalar rule guards against — an accidentally-blank variable
// silently erasing a configured value — is smaller here: setting a list
// variable to empty is a deliberate act, and the failure it prevents is a
// deployment that cannot express its own storage.
func envListInto(k string, dst *[]string) {
	v, ok := os.LookupEnv(k)
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
	// [[s3]] sites: a location and the credentials to reach it, together.
	for i, site := range c.S3Sites {
		name := strings.TrimSpace(site.Name)
		uri := strings.TrimSpace(site.URI)
		if name == "" || uri == "" {
			return nil, fmt.Errorf("[[s3]] entry %d: both name and uri are required", i+1)
		}
		if !strings.HasPrefix(uri, "s3://") {
			return nil, fmt.Errorf("[[s3]] %q: uri must be an s3:// URI", name)
		}
		id := "cfg-" + strings.ToLower(name)
		if seen[id] {
			// "default" is the likely collision: a filesystem location of that
			// name exists unless storage.paths is set, so an [[s3]] called
			// default clashes with a built-in nobody wrote.
			return nil, fmt.Errorf("storage location %q is declared twice — "+
				"[[s3]] %q collides with a storage.paths entry (there is a "+
				"built-in %q path unless storage.paths is set); give the site "+
				"another name, or set storage.paths yourself", name, name, name)
		}
		seen[id] = true
		out = append(out, loc{
			ID: id, Name: name, Path: uri, Kind: "s3",
			Default: site.Default || (len(c.StoragePaths) == 0 && len(c.StorageS3) == 0 && i == 0),
		})
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
