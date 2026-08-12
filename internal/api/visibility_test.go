package api

import (
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

func src(id, level string) catalog.Source {
	return catalog.Source{ID: id, Name: id, Version: "1", Visibility: level}
}

// The whole of the three-level model, stated as a table. Every cell here is an
// access decision, so this is the test that matters most in the change.
func TestWhoCanSeeWhat(t *testing.T) {
	anon := visibility{}
	user := visibility{signedIn: true}
	member := visibility{signedIn: true, granted: map[string]bool{"secret": true}}
	admin := visibility{admin: true, signedIn: true}

	for _, tc := range []struct {
		who   string
		v     visibility
		level string
		id    string
		want  bool
	}{
		// public: everybody, including a visitor with no account at all.
		{"anonymous", anon, catalog.VisibilityPublic, "open", true},
		{"account", user, catalog.VisibilityPublic, "open", true},
		{"admin", admin, catalog.VisibilityPublic, "open", true},

		// signed_in: the level that was missing. An account is the whole of it —
		// no grant, no team, no per-source administration.
		{"anonymous", anon, catalog.VisibilitySignedIn, "members", false},
		{"account", user, catalog.VisibilitySignedIn, "members", true},
		{"admin", admin, catalog.VisibilitySignedIn, "members", true},

		// restricted: an account is not enough; the grant is.
		{"anonymous", anon, catalog.VisibilityRestricted, "secret", false},
		{"account without a grant", user, catalog.VisibilityRestricted, "secret", false},
		{"account with a grant", member, catalog.VisibilityRestricted, "secret", true},
		{"admin", admin, catalog.VisibilityRestricted, "secret", true},
	} {
		if got := tc.v.canSee(src(tc.id, tc.level)); got != tc.want {
			t.Errorf("%s / %s: canSee = %v, want %v", tc.who, tc.level, got, tc.want)
		}
	}
}

// A grant is a stronger statement than the level it hangs off, so it should work
// on a signed_in source too — otherwise granting one to an anonymous-facing team
// would silently do nothing.
func TestAGrantAlsoSatisfiesSignedIn(t *testing.T) {
	granted := visibility{granted: map[string]bool{"members": true}}
	if !granted.canSee(src("members", catalog.VisibilitySignedIn)) {
		t.Error("a granted source was hidden because the caller was not signed in")
	}
}

// A level this binary does not know is not one it hands data out on. The schema
// can move ahead of the code during a rollout, and the safe reading of an
// unrecognized value is "no".
func TestAnUnknownLevelIsRefused(t *testing.T) {
	for _, v := range []visibility{{}, {signedIn: true}, {granted: map[string]bool{}}} {
		if v.canSee(src("odd", "some_future_level")) {
			t.Error("an unrecognized visibility was treated as visible")
		}
	}
	// Except for an administrator, who can always see everything.
	if !(visibility{admin: true}).canSee(src("odd", "some_future_level")) {
		t.Error("an administrator was refused")
	}
}

// A snapshot has no level of its own: it is exactly as open as the least open
// thing it pins. Anything else would either promise annotations the caller may
// not compute, or put an access decision in a second place.
func TestSnapshotVisibilityIsDerivedFromItsSources(t *testing.T) {
	anon := visibility{}
	user := visibility{signedIn: true}

	// All public: everybody, including a visitor with no account.
	open := catalog.Snapshot{Sources: []catalog.Source{src("a", catalog.VisibilityPublic)}}
	if !anon.canSeeSnapshot(open) {
		t.Error("an anonymous caller was refused a fully public snapshot")
	}
	if got := open.EffectiveVisibility(); got != catalog.VisibilityPublic {
		t.Errorf("EffectiveVisibility = %q, want public", got)
	}

	// One signed_in source drags the whole snapshot up with it.
	mixed := catalog.Snapshot{Sources: []catalog.Source{
		src("a", catalog.VisibilityPublic),
		src("b", catalog.VisibilitySignedIn),
	}}
	if anon.canSeeSnapshot(mixed) {
		t.Error("a snapshot exposed a signed_in source to an anonymous caller")
	}
	if !user.canSeeSnapshot(mixed) {
		t.Error("an account was refused a snapshot whose sources it can all see")
	}
	if got := mixed.EffectiveVisibility(); got != catalog.VisibilitySignedIn {
		t.Errorf("EffectiveVisibility = %q, want signed_in", got)
	}

	// The most restrictive wins, not the most recent or the first.
	strict := catalog.Snapshot{Sources: []catalog.Source{
		src("a", catalog.VisibilityRestricted),
		src("b", catalog.VisibilitySignedIn),
		src("c", catalog.VisibilityPublic),
	}}
	if got := strict.EffectiveVisibility(); got != catalog.VisibilityRestricted {
		t.Errorf("EffectiveVisibility = %q, want restricted", got)
	}
	if user.canSeeSnapshot(strict) {
		t.Error("an ungranted account saw a snapshot pinning a restricted source")
	}
}

// A snapshot pinning nothing withholds nothing, so it is public. Worth pinning
// because the fold starts at public and an empty range must not fall through to
// something stricter by accident.
func TestASnapshotPinningNothingIsPublic(t *testing.T) {
	empty := catalog.Snapshot{}
	if got := empty.EffectiveVisibility(); got != catalog.VisibilityPublic {
		t.Errorf("EffectiveVisibility = %q, want public", got)
	}
	if !(visibility{}).canSeeSnapshot(empty) {
		t.Error("an empty snapshot was hidden from an anonymous caller")
	}
}

func TestMostRestrictiveWins(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{catalog.VisibilityPublic, catalog.VisibilityPublic, catalog.VisibilityPublic},
		{catalog.VisibilityPublic, catalog.VisibilitySignedIn, catalog.VisibilitySignedIn},
		{catalog.VisibilitySignedIn, catalog.VisibilityPublic, catalog.VisibilitySignedIn},
		{catalog.VisibilitySignedIn, catalog.VisibilityRestricted, catalog.VisibilityRestricted},
		{catalog.VisibilityRestricted, catalog.VisibilityPublic, catalog.VisibilityRestricted},
	} {
		if got := catalog.MostRestrictive(tc.a, tc.b); got != tc.want {
			t.Errorf("MostRestrictive(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestVisibilityRequestParsing(t *testing.T) {
	for in, want := range map[string]string{
		"public":     catalog.VisibilityPublic,
		"signed_in":  catalog.VisibilitySignedIn,
		"restricted": catalog.VisibilityRestricted,
		"  PUBLIC ":  catalog.VisibilityPublic,
		// The name restricted used to have, so an existing script keeps working
		// and keeps meaning what it meant.
		"private": catalog.VisibilityRestricted,
	} {
		got, err := visibilityRequest{Visibility: in}.parse()
		if err != nil {
			t.Errorf("parse(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parse(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "everyone", "public ish", "signed-in"} {
		if _, err := (visibilityRequest{Visibility: bad}).parse(); err == nil {
			t.Errorf("parse(%q) was accepted", bad)
		}
	}
}
