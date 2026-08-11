package catalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compgenlab/varianthub-web/internal/config"
)

// Site is the deployment's own settings: the decisions an administrator makes
// about this installation rather than about any one source.
//
// config.toml supplies the values; a row in site_setting overrides one. The
// point of the override is that changing a setting should not mean editing a
// file on a host and restarting — but the file stays authoritative for a fresh
// deployment, so an installation can still be described by its configuration.
type Site struct {
	// AllowAnonymous lets a caller with no credential use the annotation flow.
	AllowAnonymous bool `json:"allow_anonymous"`
	// CacheEnabled turns the shared annotation cache on for new jobs. Off means
	// jobs compute every locus and persist nothing; entries already cached are
	// left alone, so this is reversible without losing the cache.
	CacheEnabled bool `json:"cache_enabled"`
	// CacheMaxEntries caps cached (variant, source) units. 0 is unbounded.
	CacheMaxEntries int64 `json:"cache_max_entries"`
	// CacheMaxAge discards entries unused for longer than this. 0 is unbounded.
	CacheMaxAge time.Duration `json:"-"`
	// CacheMaxAgeText is CacheMaxAge as written ("2160h"), for the API and form.
	CacheMaxAgeText string `json:"cache_max_age"`

	// Service limits, per tier. Rates are per hour; see Limits.
	AnonConcurrent     int `json:"anon_concurrent"`
	AnonPerHour        int `json:"anon_per_hour"`
	StandardConcurrent int `json:"standard_concurrent"`
	StandardPerHour    int `json:"standard_per_hour"`
	ElevatedConcurrent int `json:"elevated_concurrent"`
	ElevatedPerHour    int `json:"elevated_per_hour"`
}

// SiteFromConfig is the deployment as configured, before any stored override.
//
// One implementation, called by both the API and the worker. Two would drift:
// a setting added to one and forgotten in the other reads as configured in the
// process that serves the form and as unset in the process that acts on it,
// which looks like the setting not working rather than like a missing line.
func SiteFromConfig(cfg *config.Config) Site {
	d := Site{
		AllowAnonymous:  cfg.AllowAnonymous,
		CacheEnabled:    cfg.CacheEnabled,
		CacheMaxEntries: cfg.CacheMaxEntries,

		AnonConcurrent:     cfg.AnonConcurrent,
		AnonPerHour:        cfg.AnonPerHour,
		StandardConcurrent: cfg.StandardConcurrent,
		StandardPerHour:    cfg.StandardPerHour,
		ElevatedConcurrent: cfg.ElevatedConcurrent,
		ElevatedPerHour:    cfg.ElevatedPerHour,
	}
	// Through the same parser the overrides use, so "2160h" cannot mean one thing
	// in the file and another in the form.
	_ = (&d).ApplySetting(KeyCacheMaxAge, cfg.CacheMaxAge)
	return d
}

// Setting keys. Named here so the API, the form and the parser cannot drift:
// every one of them goes through this list.
const (
	KeyAllowAnonymous  = "allow_anonymous"
	KeyCacheEnabled    = "cache_enabled"
	KeyCacheMaxEntries = "cache_max_entries"
	KeyCacheMaxAge     = "cache_max_age"

	KeyAnonConcurrent     = "anon_concurrent"
	KeyAnonPerHour        = "anon_per_hour"
	KeyStandardConcurrent = "standard_concurrent"
	KeyStandardPerHour    = "standard_per_hour"
	KeyElevatedConcurrent = "elevated_concurrent"
	KeyElevatedPerHour    = "elevated_per_hour"
)

// Service tiers: how much of the pool an account may occupy.
//
// Deliberately not the same axis as role. Role is permission — what you may
// administer — and this is capacity. Sharing one field would mean raising
// someone's limits by making them an administrator.
const (
	TierStandard  = "standard"
	TierElevated  = "elevated"
	TierUnlimited = "unlimited"
)

// Tiers is every assignable tier, for a form that should not keep its own list.
var Tiers = []string{TierStandard, TierElevated, TierUnlimited}

// ValidTier reports whether a tier is one this server knows.
//
// An unknown tier resolves to the standard limits rather than to none, so a
// hand-edited row cannot promote an account by misspelling it.
func ValidTier(t string) bool {
	for _, k := range Tiers {
		if k == t {
			return true
		}
	}
	return false
}

// Limits are what one caller may ask of the service.
//
// Rates are per hour, where the per-IP submit rate in config is per minute. The
// unit differs because the question does: an anonymous visitor should be able
// to run something every few minutes, which per-minute integers cannot say at
// all. Zero means unbounded, the same convention CacheMaxEntries uses.
type Limits struct {
	// Concurrent caps running jobs for one caller. Enforced at dispatch, so an
	// over-limit job waits rather than being refused.
	Concurrent int
	// PerHour caps submissions. Enforced at the door, because a request that
	// will be refused should not become a row first.
	PerHour int
}

// Unlimited reports whether nothing is capped.
func (l Limits) Unlimited() bool { return l.Concurrent <= 0 && l.PerHour <= 0 }

// LimitsFor resolves what a tier allows. An unrecognized tier gets the standard
// limits — the safe direction, since the alternative is that a typo grants more
// than any tier was meant to.
func (s Site) LimitsFor(tier string) Limits {
	switch tier {
	case TierUnlimited:
		return Limits{}
	case TierElevated:
		return Limits{Concurrent: s.ElevatedConcurrent, PerHour: s.ElevatedPerHour}
	default:
		return Limits{Concurrent: s.StandardConcurrent, PerHour: s.StandardPerHour}
	}
}

// AnonLimits are what a visitor who has not signed in may ask for.
//
// Lower than any account's, and separate from the tier list because anonymity
// is not a tier an administrator assigns — there is nobody to assign it to. It
// is what the absence of an account gets.
func (s Site) AnonLimits() Limits {
	return Limits{Concurrent: s.AnonConcurrent, PerHour: s.AnonPerHour}
}

// SettingKeys is every overridable key, in the order a form should show them.
//
// Derived from nothing else and checked by a test against the apply/serialize
// pair, because a key added to one and not the others is silently ignored — the
// form saves it, the reader never looks, and the setting appears not to work.
var SettingKeys = []string{
	KeyAllowAnonymous, KeyCacheEnabled, KeyCacheMaxEntries, KeyCacheMaxAge,
	KeyAnonConcurrent, KeyAnonPerHour,
	KeyStandardConcurrent, KeyStandardPerHour,
	KeyElevatedConcurrent, KeyElevatedPerHour,
}

// Values renders a Site as the override map, the inverse of apply.
func (s Site) Values() map[string]string {
	return map[string]string{
		KeyAllowAnonymous:  strconv.FormatBool(s.AllowAnonymous),
		KeyCacheEnabled:    strconv.FormatBool(s.CacheEnabled),
		KeyCacheMaxEntries: strconv.FormatInt(s.CacheMaxEntries, 10),
		KeyCacheMaxAge:     s.CacheMaxAgeText,

		KeyAnonConcurrent:     strconv.Itoa(s.AnonConcurrent),
		KeyAnonPerHour:        strconv.Itoa(s.AnonPerHour),
		KeyStandardConcurrent: strconv.Itoa(s.StandardConcurrent),
		KeyStandardPerHour:    strconv.Itoa(s.StandardPerHour),
		KeyElevatedConcurrent: strconv.Itoa(s.ElevatedConcurrent),
		KeyElevatedPerHour:    strconv.Itoa(s.ElevatedPerHour),
	}
}

// ApplySetting overlays one override onto a Site, returning an error the caller
// can show rather than silently keeping the default. Exported so configured
// defaults are parsed by the same code as stored overrides — two parsers for one
// value is how "2160h" comes to mean different things in two places.
func (s *Site) ApplySetting(key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case KeyAllowAnonymous, KeyCacheEnabled:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s: %q is not true or false", key, value)
		}
		if key == KeyAllowAnonymous {
			s.AllowAnonymous = b
		} else {
			s.CacheEnabled = b
		}
	case KeyCacheMaxEntries:
		if value == "" {
			s.CacheMaxEntries = 0
			return nil
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("%s: %q is not a non-negative whole number", key, value)
		}
		s.CacheMaxEntries = n
	case KeyAnonConcurrent, KeyAnonPerHour,
		KeyStandardConcurrent, KeyStandardPerHour,
		KeyElevatedConcurrent, KeyElevatedPerHour:
		// 0 is unbounded rather than "refuse everything", so an operator cannot
		// take the service down by clearing a field.
		n, err := strconv.Atoi(value)
		if value == "" {
			n, err = 0, nil
		}
		if err != nil || n < 0 {
			return fmt.Errorf("%s: %q is not a non-negative whole number", key, value)
		}
		switch key {
		case KeyAnonConcurrent:
			s.AnonConcurrent = n
		case KeyAnonPerHour:
			s.AnonPerHour = n
		case KeyStandardConcurrent:
			s.StandardConcurrent = n
		case KeyStandardPerHour:
			s.StandardPerHour = n
		case KeyElevatedConcurrent:
			s.ElevatedConcurrent = n
		case KeyElevatedPerHour:
			s.ElevatedPerHour = n
		}
	case KeyCacheMaxAge:
		if value == "" {
			s.CacheMaxAge, s.CacheMaxAgeText = 0, ""
			return nil
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s: %q is not a positive duration (e.g. \"2160h\")", key, value)
		}
		s.CacheMaxAge, s.CacheMaxAgeText = d, value
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	return nil
}

// ValidateSettings reports whether a set of overrides is usable, without
// applying them — so a form can be rejected as a whole rather than half-saved.
//
// An empty value is "revert to what is configured", not a value to parse. It has
// to be excused here or a setting could never be handed back to the file: a
// boolean override would be unclearable, since "" is neither true nor false.
func ValidateSettings(values map[string]string) error {
	var probe Site
	for k, v := range values {
		if strings.TrimSpace(v) == "" {
			if !knownSetting(k) {
				return fmt.Errorf("unknown setting %q", k)
			}
			continue
		}
		if err := probe.ApplySetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

func knownSetting(key string) bool {
	for _, k := range SettingKeys {
		if k == key {
			return true
		}
	}
	return false
}

// siteTTL bounds how stale an effective setting can be.
//
// The API and the worker are separate processes, so a change made through the
// admin form reaches the worker only when the worker next reads. A short TTL is
// how it finds out, and it costs one small query per process per interval
// instead of one per request on the authentication path. Nothing here is
// safety-critical at second granularity: the worst case is one job materialized
// with the previous cache setting.
const siteTTL = 5 * time.Second

type siteCache struct {
	mu     sync.Mutex
	at     time.Time
	values map[string]string
}

// SiteSettings returns the stored overrides, cached for siteTTL.
func (s *Store) SiteSettings(ctx context.Context) (map[string]string, error) {
	s.site.mu.Lock()
	defer s.site.mu.Unlock()
	if s.site.values != nil && time.Since(s.site.at) < siteTTL {
		return s.site.values, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM site_setting`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.site.values, s.site.at = out, time.Now()
	return out, nil
}

// EffectiveSite resolves defaults against the stored overrides.
//
// A bad stored value is skipped rather than fatal, and the default stands. The
// alternative is an installation that cannot start because a setting written
// months ago no longer parses — a worse outcome than running as configured.
// Writes are validated, so this should not happen; it is the floor under a
// hand-edited row.
func (s *Store) EffectiveSite(ctx context.Context, defaults Site) (Site, error) {
	over, err := s.SiteSettings(ctx)
	if err != nil {
		return defaults, err
	}
	out := defaults
	for _, k := range SettingKeys {
		v, ok := over[k]
		if !ok {
			continue
		}
		_ = out.ApplySetting(k, v)
	}
	return out, nil
}

// PutSiteSettings records overrides, deleting the ones set back to empty so
// "same as configured" stays a single state.
//
// All or nothing: the values are validated before any is written, so a form with
// one bad field does not leave the others applied.
func (s *Store) PutSiteSettings(ctx context.Context, values map[string]string) error {
	if err := ValidateSettings(values); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	now := s.nowFn()
	for k, v := range values {
		if strings.TrimSpace(v) == "" {
			if _, err := tx.Exec(ctx, `DELETE FROM site_setting WHERE key=$1`, k); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO site_setting (key, value, updated_at) VALUES ($1,$2,$3)
			ON CONFLICT (key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			k, strings.TrimSpace(v), now); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// This process should not serve the old value back to the administrator who
	// just changed it; other processes catch up within siteTTL.
	s.site.mu.Lock()
	s.site.values = nil
	s.site.mu.Unlock()
	return nil
}
