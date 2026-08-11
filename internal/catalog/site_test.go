package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/compgenlab/varianthub-web/internal/config"
)

// Every overridable key must round-trip through both halves. A key listed but
// unreadable is saved and ignored — the form appears to work and the setting
// never applies — and a key readable but unlisted cannot be set at all.
func TestEverySettingKeyRoundTrips(t *testing.T) {
	full := Site{
		AllowAnonymous:  true,
		CacheEnabled:    true,
		CacheMaxEntries: 12345,
		CacheMaxAge:     48 * time.Hour,
		CacheMaxAgeText: "48h",
	}
	values := full.Values()

	for _, k := range SettingKeys {
		if _, ok := values[k]; !ok {
			t.Errorf("%s is settable but Values() does not emit it, so it can never "+
				"be shown or saved back", k)
		}
	}
	for k := range values {
		found := false
		for _, want := range SettingKeys {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Values() emits %s but it is not in SettingKeys, so nothing reads it", k)
		}
	}

	// And the pair actually inverts: apply(Values(x)) == x.
	var back Site
	for k, v := range values {
		if err := back.ApplySetting(k, v); err != nil {
			t.Fatalf("applying %s=%q: %v", k, v, err)
		}
	}
	if back != full {
		t.Errorf("round trip gave %+v, want %+v", back, full)
	}
}

func TestSettingsRejectUnusableValues(t *testing.T) {
	cases := map[string]string{
		KeyAllowAnonymous:  "yes please",
		KeyCacheMaxEntries: "-1",
		KeyCacheMaxAge:     "90 days",
	}
	for k, v := range cases {
		if err := ValidateSettings(map[string]string{k: v}); err == nil {
			t.Errorf("%s=%q was accepted; it would be stored and silently ignored", k, v)
		}
	}
	if err := ValidateSettings(map[string]string{"nonsense": "1"}); err == nil {
		t.Error("an unknown key was accepted, so a typo in the form saves nothing and says so")
	}
}

// The stored value wins over the file, and clearing it hands the setting back to
// the file rather than to zero.
func TestOverrideBeatsConfigAndRevertsToIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	defaults := SiteFromConfig(&config.Config{
		AllowAnonymous: false,
		CacheEnabled:   true,
		CacheMaxAge:    "2160h",
	})

	got, err := s.EffectiveSite(ctx, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got.AllowAnonymous || !got.CacheEnabled || got.CacheMaxAge != 2160*time.Hour {
		t.Fatalf("with no overrides, got %+v, want the configured defaults", got)
	}

	if err := s.PutSiteSettings(ctx, map[string]string{
		KeyAllowAnonymous: "true",
		KeyCacheEnabled:   "false",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.EffectiveSite(ctx, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowAnonymous || got.CacheEnabled {
		t.Errorf("override ignored: got %+v", got)
	}
	// Untouched settings still come from the file.
	if got.CacheMaxAge != 2160*time.Hour {
		t.Errorf("an unrelated setting changed: %v", got.CacheMaxAge)
	}

	// Empty clears, and the file's value returns.
	if err := s.PutSiteSettings(ctx, map[string]string{KeyAllowAnonymous: ""}); err != nil {
		t.Fatal(err)
	}
	got, err = s.EffectiveSite(ctx, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got.AllowAnonymous {
		t.Error("clearing an override did not hand the setting back to the configuration")
	}
}

// One bad field must not leave the others applied: an administrator who mistypes
// a duration should get the form back, not half a configuration.
func TestABadFieldSavesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	err := s.PutSiteSettings(ctx, map[string]string{
		KeyAllowAnonymous: "true",
		KeyCacheMaxAge:    "ninety days",
	})
	if err == nil {
		t.Fatal("an unparseable duration was accepted")
	}
	over, err := s.SiteSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := over[KeyAllowAnonymous]; ok {
		t.Error("the valid half of a rejected form was written anyway")
	}
}
