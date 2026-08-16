package config

import (
	"strings"
	"testing"
)

// The distinction this exists for. cors_origins is empty in the ordinary
// deployment — the app is served from the same origin as the API, so there is
// no cross-origin call to allow — while the object store is a different host in
// every deployment and always needs the web origin named.
//
// Deriving the bucket rule from CORSOrigins produced an empty list for exactly
// the installation that most needed the rule, and the symptom was a download
// button that did nothing.
func TestTheBucketOriginIsTheAppsNotTheAPIsCORSList(t *testing.T) {
	cases := []struct {
		name   string
		public string
		cors   []string
		want   []string
	}{
		{
			"same-origin deployment: no API CORS, and still an origin to allow",
			"https://variants.example.org", nil,
			[]string{"https://variants.example.org"},
		},
		{
			"a separate front end calls the API, and downloads through it too",
			"https://variants.example.org", []string{"https://app.example.org"},
			[]string{"https://variants.example.org", "https://app.example.org"},
		},
		{
			"the app's own origin listed twice is one origin",
			"https://variants.example.org", []string{"https://variants.example.org"},
			[]string{"https://variants.example.org"},
		},
		{
			"a trailing slash is the same origin",
			"https://variants.example.org", []string{"https://variants.example.org/"},
			[]string{"https://variants.example.org"},
		},
		{
			// A wildcard is a fine answer for an API and a bad one for a bucket
			// rule: it would be written literally and allow every origin there
			// is. Dropped rather than passed through.
			"a wildcard API origin is not a bucket origin",
			"https://variants.example.org", []string{"*"},
			[]string{"https://variants.example.org"},
		},
		{
			"nothing configured yields nothing, so the caller can say so",
			"", nil, nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{PublicURL: c.public, CORSOrigins: c.cors}
			got := cfg.DownloadOrigins()
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("DownloadOrigins() = %v, want %v", got, c.want)
			}
		})
	}
}
