package identity

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "correct horse") {
		t.Error("the hash contains the password")
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("the right password did not match")
	}
	if CheckPassword(h, "wrong") {
		t.Error("the wrong password matched")
	}
	// An account that authenticates elsewhere has no password, and must not be
	// unlocked by presenting the empty string.
	if CheckPassword("", "") {
		t.Error("an empty stored hash accepted an empty password")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Error("a too-short password was accepted")
	}
}

func TestTokenLifecycle(t *testing.T) {
	secret, prefix, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Errorf("secret lacks the scannable prefix: %q", secret)
	}
	if !strings.HasPrefix(prefix, TokenPrefix) || len(prefix) >= len(secret) {
		t.Errorf("prefix %q is not a short, distinctive lead of the secret", prefix)
	}
	// The stored hash must not reveal the secret.
	if strings.Contains(hash, secret[len(TokenPrefix):]) {
		t.Error("the stored hash contains the secret")
	}
	if !TokenMatches(hash, secret) {
		t.Error("the minted token did not match its own hash")
	}
	other, _, _, _ := NewToken()
	if TokenMatches(hash, other) {
		t.Error("a different token matched")
	}
	// A presented token must be locatable by prefix before its hash is checked.
	got, ok := PrefixOf(secret)
	if !ok || got != prefix {
		t.Errorf("PrefixOf = %q, %v; want %q", got, ok, prefix)
	}
	if _, ok := PrefixOf("not-a-varhub-token"); ok {
		t.Error("PrefixOf accepted a foreign token")
	}
	// Two mints must not collide.
	a, _, _, _ := NewToken()
	b, _, _, _ := NewToken()
	if a == b {
		t.Error("two tokens minted identically")
	}
}

func TestCallerAuthority(t *testing.T) {
	admin := Caller{User: &User{ID: "u1", Email: "a@x", Role: RoleAdmin}}
	member := Caller{User: &User{ID: "u2", Email: "m@x", Role: RoleMember}, TeamIDs: []string{"t1"}}
	svc := Caller{Service: true}
	anon := Caller{}

	for _, tc := range []struct {
		name  string
		c     Caller
		admin bool
		anon  bool
	}{
		{"admin user", admin, true, false},
		{"member", member, false, false},
		// The service account holds the deployment's own key. Demoting it would
		// lock an operator out of the API they installed.
		{"service", svc, true, false},
		{"anonymous", anon, false, true},
	} {
		if got := tc.c.IsAdmin(); got != tc.admin {
			t.Errorf("%s: IsAdmin = %v, want %v", tc.name, got, tc.admin)
		}
		if got := tc.c.Anonymous(); got != tc.anon {
			t.Errorf("%s: Anonymous = %v, want %v", tc.name, got, tc.anon)
		}
	}

	if !member.InTeam("t1") || member.InTeam("t2") {
		t.Error("InTeam is wrong")
	}
	if anon.UserID() != "" || member.UserID() != "u2" {
		t.Error("UserID is wrong")
	}
	if svc.Label() != "service" || member.Label() != "m@x" {
		t.Errorf("labels: %q %q", svc.Label(), member.Label())
	}
}

func TestNormalizeEmail(t *testing.T) {
	// The unique index is on lower(email); normalizing anywhere else would let
	// two accounts exist for one person and split their grants.
	for _, in := range []string{"A@X.ORG", " a@x.org ", "a@X.org"} {
		if got := NormalizeEmail(in); got != "a@x.org" {
			t.Errorf("NormalizeEmail(%q) = %q", in, got)
		}
	}
}
