package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/compgenlab/varianthub-web/internal/identity"
)

// stubProvider is a minimal OIDC provider: it hands back one fixed set of
// claims for any code. Enough to drive the callback end to end, which is where
// every decision this feature makes actually lives.
func stubProvider(t *testing.T, claims oidcClaims) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "stub-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer stub-access-token" {
			t.Errorf("userinfo called with %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claims)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// withOIDC points a harness at a stub provider.
func (h *harness) withOIDC(t *testing.T, claims oidcClaims, autoProvision ...string) {
	t.Helper()
	stub := stubProvider(t, claims)
	h.server.oidc = &oidcProvider{
		name: identity.ProviderCILogon,
		oauth: &oauth2.Config{
			ClientID: "cid", ClientSecret: "secret",
			RedirectURL: "http://vh.example/auth/cilogon/callback",
			Endpoint:    oauth2.Endpoint{AuthURL: stub.URL + "/authorize", TokenURL: stub.URL + "/token"},
			Scopes:      []string{"openid", "email", "profile"},
		},
		userInfoURL:   stub.URL + "/userinfo",
		autoProvision: autoProvision,
		defaultRole:   identity.RoleMember,
	}
	h.http = h.server.Routes()
}

// signIn drives the whole redirect round trip and returns the callback's
// response, so state handling is exercised rather than bypassed.
func (h *harness) signIn(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, httptest.NewRequest("GET", "/auth/cilogon", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("login redirect = %d, want 302", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("no state in the authorize URL")
	}

	cb := httptest.NewRequest("GET", "/auth/cilogon/callback?code=abc&state="+state, nil)
	for _, c := range w.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	h.http.ServeHTTP(out, cb)
	return out
}

func sessionFrom(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// The invitation flow: an administrator creates the account with no password,
// and the first CILogon sign-in claims it.
func TestCILogonLinksToAnInvitedAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	invited, err := h.ids.CreateUser(ctx, "researcher@iu.edu", "", identity.RoleMember, "")
	if err != nil {
		t.Fatal(err)
	}
	if !invited.SSO {
		t.Error("an account created with no password is not marked SSO")
	}

	h.withOIDC(t, oidcClaims{Sub: "http://cilogon.org/serverA/users/1", Email: "researcher@iu.edu", Name: "A Researcher"})
	w := h.signIn(t)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d, want 303 (%s)", w.Code, w.Body.String())
	}
	sess := sessionFrom(w)
	if sess == "" {
		t.Fatal("callback issued no session cookie")
	}

	// The session is a real one for the invited account.
	got, err := h.ids.UserBySession(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != invited.ID {
		t.Errorf("signed in as %q, want the invited account %q", got.Email, invited.Email)
	}

	// The link is keyed on the subject, so a later email change still returns
	// to the same account rather than creating a second one.
	h.withOIDC(t, oidcClaims{Sub: "http://cilogon.org/serverA/users/1", Email: "moved@elsewhere.edu"})
	w = h.signIn(t)
	again, err := h.ids.UserBySession(ctx, sessionFrom(w))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != invited.ID {
		t.Errorf("a changed email created a new account (%q)", again.Email)
	}
	users, _ := h.ids.ListUsers(ctx)
	if len(users) != 1 {
		t.Errorf("account count = %d, want 1", len(users))
	}
}

// CILogon federates thousands of institutions, so authenticating there is not a
// reason to have an account here. Invite-only is the default — but a stranger
// is now queued for review rather than turned away with nothing to do.
func TestCILogonWaitlistsAnUnknownIdentity(t *testing.T) {
	h := newHarness(t)
	h.withOIDC(t, oidcClaims{Sub: "sub-stranger", Email: "stranger@example.org"})
	ctx := context.Background()

	w := h.signIn(t)
	// The two that matter: authenticating is not being admitted.
	if got := sessionFrom(w); got != "" {
		t.Fatal("a stranger was issued a session")
	}
	users, _ := h.ids.ListUsers(ctx)
	if len(users) != 0 {
		t.Errorf("an account was created anyway: %+v", users)
	}

	if loc := w.Header().Get("Location"); !strings.Contains(loc, "sso_waitlisted") {
		t.Errorf("redirect = %q, want the waitlist notice", loc)
	}
	reqs, err := h.ids.ListAccessRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 {
		t.Fatalf("got %d access requests, want 1", len(reqs))
	}
	if reqs[0].Email != "stranger@example.org" || reqs[0].Status != identity.RequestPending {
		t.Errorf("request = %+v, want a pending one for the verified address", reqs[0])
	}
}

// An allow-listed domain provisions on first sign-in — as a member, never an
// administrator.
func TestCILogonAutoProvisionsAllowedDomains(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.withOIDC(t, oidcClaims{Sub: "sub-new", Email: "newcomer@umail.iu.edu", Name: "New Comer"}, "iu.edu")

	w := h.signIn(t)
	sess := sessionFrom(w)
	if sess == "" {
		t.Fatalf("no session issued (%s)", w.Header().Get("Location"))
	}
	u, err := h.ids.UserBySession(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	// A subdomain of an allow-listed domain counts: institutions routinely
	// issue mail on one.
	if u.Email != "newcomer@umail.iu.edu" {
		t.Errorf("provisioned %q", u.Email)
	}
	if u.Role != identity.RoleMember {
		t.Errorf("provisioned with role %q; having the right email must never grant administration", u.Role)
	}
	if !u.SSO {
		t.Error("a provisioned SSO account has a local password")
	}

	// A domain that merely ends in the allowed string is not a subdomain of it.
	h2 := newHarness(t)
	h2.withOIDC(t, oidcClaims{Sub: "sub-evil", Email: "attacker@notiu.edu"}, "iu.edu")
	if got := sessionFrom(h2.signIn(t)); got != "" {
		t.Error("notiu.edu was treated as a subdomain of iu.edu")
	}
}

// A disabled account does not come back through the side door.
func TestCILogonRefusesDisabledAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u, err := h.ids.CreateUser(ctx, "gone@iu.edu", "", identity.RoleMember, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ids.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	h.withOIDC(t, oidcClaims{Sub: "sub-gone", Email: "gone@iu.edu"}, "iu.edu")

	w := h.signIn(t)
	if sessionFrom(w) != "" {
		t.Fatal("a disabled account signed in through CILogon")
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "sso_disabled") {
		t.Errorf("redirect = %q, want sso_disabled", loc)
	}
}

// The state cookie is what stops someone else's callback being replayed into
// this browser as a login.
func TestCILogonRequiresMatchingState(t *testing.T) {
	h := newHarness(t)
	h.withOIDC(t, oidcClaims{Sub: "sub-1", Email: "a@iu.edu"}, "iu.edu")

	for _, tc := range []struct {
		name          string
		cookie, param string
	}{
		{"no cookie", "", "abc"},
		{"no parameter", "abc", ""},
		{"mismatched", "abc", "def"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/auth/cilogon/callback?code=x&state="+tc.param, nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			h.http.ServeHTTP(w, r)
			if sessionFrom(w) != "" {
				t.Fatal("a callback with bad state issued a session")
			}
			if loc := w.Header().Get("Location"); !strings.Contains(loc, "sso_state") {
				t.Errorf("redirect = %q, want sso_state", loc)
			}
		})
	}
}

// `next` must not turn sign-in into an open redirect.
func TestSafeNextPath(t *testing.T) {
	for in, want := range map[string]string{
		"/jobs":                   "/jobs",
		"/":                       "/",
		"/admin/people?tab=teams": "/admin/people?tab=teams",
		"//evil.example":          "", // protocol-relative: the browser leaves the site
		"https://evil.example":    "",
		"javascript:alert(1)":     "",
		"":                        "",
		"jobs":                    "",
	} {
		if got := safeNextPath(in); got != want {
			t.Errorf("safeNextPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// An account reached through SSO still has no password here, so the password
// endpoints keep refusing it.
func TestCILogonAccountStaysPasswordless(t *testing.T) {
	h := newHarness(t)
	h.withOIDC(t, oidcClaims{Sub: "sub-x", Email: "x@iu.edu"}, "iu.edu")
	sess := sessionFrom(h.signIn(t))
	if sess == "" {
		t.Fatal("no session")
	}

	r := httptest.NewRequest("POST", "/api/v1/auth/password",
		strings.NewReader(`{"current_password":"x","new_password":"something-long"}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: sess})
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("password change on an SSO account = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}
