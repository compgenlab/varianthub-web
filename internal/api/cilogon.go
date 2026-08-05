package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/identity"
)

// CILogon sign-in.
//
// CILogon federates several thousand institutional identity providers, so a
// successful login proves only that *some* institution vouched for the person —
// not that they belong here. Account creation is therefore deliberate: either an
// administrator created the account already (and this links to it by email), or
// the verified address is under a domain the deployment allow-listed. Anything
// else is refused with an explanation rather than silently provisioned.

const (
	// CILogon's endpoints. Constants rather than discovery: they have been
	// stable for years, and a discovery fetch at startup is one more thing that
	// can make the server fail to boot.
	cilogonAuthURL     = "https://cilogon.org/authorize"
	cilogonTokenURL    = "https://cilogon.org/oauth2/token"
	cilogonUserInfoURL = "https://cilogon.org/oauth2/userinfo"

	// Cookies carrying state across the round trip. Short-lived and scoped to
	// the callback path, so they exist only for the seconds they are needed.
	oidcStateCookie = "vh_oidc_state"
	oidcNextCookie  = "vh_oidc_next"
	oidcRoundTrip   = 5 * time.Minute
)

// oidcProvider is a configured external sign-in.
type oidcProvider struct {
	name        string
	oauth       *oauth2.Config
	userInfoURL string
	// autoProvision are email domains whose verified holders get an account on
	// first sign-in. Empty means invite-only: an administrator creates the
	// account first and this links to it.
	autoProvision []string
	// defaultRole is what an auto-provisioned account gets. Always member:
	// administration is granted deliberately, never by having the right email.
	defaultRole string
}

// newCILogon builds the provider, or nil when it is not configured.
func newCILogon(cfg *config.Config) *oidcProvider {
	if cfg.CILogonClientID == "" || cfg.CILogonClientSecret == "" || cfg.CILogonRedirectURL == "" {
		return nil
	}
	domains := make([]string, 0, len(cfg.CILogonAutoProvision))
	for _, d := range cfg.CILogonAutoProvision {
		if d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(d, "@"))); d != "" {
			domains = append(domains, d)
		}
	}
	return &oidcProvider{
		name: identity.ProviderCILogon,
		oauth: &oauth2.Config{
			ClientID:     cfg.CILogonClientID,
			ClientSecret: cfg.CILogonClientSecret,
			RedirectURL:  cfg.CILogonRedirectURL,
			Endpoint:     oauth2.Endpoint{AuthURL: cilogonAuthURL, TokenURL: cilogonTokenURL},
			Scopes:       []string{"openid", "email", "profile"},
		},
		userInfoURL:   cilogonUserInfoURL,
		autoProvision: domains,
		defaultRole:   identity.RoleMember,
	}
}

// autoProvisions reports whether a verified address is under an allow-listed
// domain. A configured "iu.edu" covers "umail.iu.edu" too, since institutions
// routinely issue mail on a subdomain.
//
// Only ever called with an address the provider verified — matching on a
// self-asserted address would make the allow-list meaningless.
func (p *oidcProvider) autoProvisions(email string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range p.autoProvision {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// handleOIDCLogin sends the browser to the provider.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.Redirect(w, r, "/?error=sso_not_configured", http.StatusSeeOther)
		return
	}
	state, err := randomState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	// The state is held in a cookie and echoed by the provider; comparing them
	// on return is what stops someone else's callback being replayed into this
	// browser as a login.
	http.SetCookie(w, s.oidcCookie(r, oidcStateCookie, state, oidcRoundTrip))
	if next := safeNextPath(r.URL.Query().Get("next")); next != "" {
		http.SetCookie(w, s.oidcCookie(r, oidcNextCookie, next, oidcRoundTrip))
	}
	http.Redirect(w, r, s.oidc.oauth.AuthCodeURL(state), http.StatusFound)
}

// handleOIDCCallback completes sign-in and issues a session.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil || s.identity == nil {
		http.Redirect(w, r, "/?error=sso_not_configured", http.StatusSeeOther)
		return
	}
	next := "/"
	if c, err := r.Cookie(oidcNextCookie); err == nil {
		if p := safeNextPath(c.Value); p != "" {
			next = p
		}
	}
	// Clear both regardless of outcome: a failed attempt must not leave a state
	// value that a later callback could satisfy.
	http.SetCookie(w, s.oidcCookie(r, oidcStateCookie, "", 0))
	http.SetCookie(w, s.oidcCookie(r, oidcNextCookie, "", 0))

	state, err := r.Cookie(oidcStateCookie)
	if err != nil || state.Value == "" || state.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/?error=sso_state", http.StatusSeeOther)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		log.Printf("api: %s returned %q", s.oidc.name, e)
		http.Redirect(w, r, "/?error=sso_denied", http.StatusSeeOther)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/?error=sso_no_code", http.StatusSeeOther)
		return
	}

	claims, err := s.oidc.exchange(r.Context(), code)
	if err != nil {
		log.Printf("api: %s sign-in failed: %v", s.oidc.name, err)
		http.Redirect(w, r, "/?error=sso_exchange", http.StatusSeeOther)
		return
	}

	user, err := s.resolveOIDCUser(r.Context(), claims)
	switch {
	case errors.Is(err, errNoAccount):
		// Named separately from a failure: the person authenticated fine and
		// simply has no account here, which an administrator fixes.
		log.Printf("api: %s sign-in by %q has no account here", s.oidc.name, claims.Email)
		http.Redirect(w, r, "/?error=sso_no_account", http.StatusSeeOther)
		return
	case errors.Is(err, errAccountDisabled):
		http.Redirect(w, r, "/?error=sso_disabled", http.StatusSeeOther)
		return
	case err != nil:
		log.Printf("api: resolving %s identity: %v", s.oidc.name, err)
		http.Redirect(w, r, "/?error=sso_internal", http.StatusSeeOther)
		return
	}

	id, exp, err := s.identity.CreateSession(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/?error=sso_session", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, s.sessionCookie(r, id, time.Unix(exp, 0)))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

var (
	errNoAccount       = errors.New("no account for this identity")
	errAccountDisabled = errors.New("account is disabled")
)

// oidcClaims is the subset of userinfo we use.
type oidcClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// exchange trades the authorization code for the caller's claims.
func (p *oidcProvider) exchange(ctx context.Context, code string) (oidcClaims, error) {
	token, err := p.oauth.Exchange(ctx, code)
	if err != nil {
		return oidcClaims{}, fmt.Errorf("token exchange: %w", err)
	}
	resp, err := p.oauth.Client(ctx, token).Get(p.userInfoURL)
	if err != nil {
		return oidcClaims{}, fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcClaims{}, fmt.Errorf("userinfo: %s", resp.Status)
	}
	var claims oidcClaims
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return oidcClaims{}, fmt.Errorf("userinfo decode: %w", err)
	}
	if claims.Sub == "" {
		// Without a subject there is nothing stable to key the account on, and
		// falling back to the email would let a provider move an account by
		// changing what it reports.
		return oidcClaims{}, errors.New("userinfo carried no subject claim")
	}
	claims.Email = identity.NormalizeEmail(claims.Email)
	return claims, nil
}

// resolveOIDCUser finds or creates the account behind a set of claims.
func (s *Server) resolveOIDCUser(ctx context.Context, claims oidcClaims) (identity.User, error) {
	// 1. A returning person: matched on the provider's subject, which survives
	//    an email change.
	u, err := s.identity.UserByIdentity(ctx, s.oidc.name, claims.Sub)
	if err == nil {
		if u.Disabled {
			return identity.User{}, errAccountDisabled
		}
		if err := s.identity.TouchIdentity(ctx, s.oidc.name, claims.Sub); err != nil {
			log.Printf("api: touching identity: %v", err)
		}
		return u, nil
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return identity.User{}, err
	}
	if claims.Email == "" {
		return identity.User{}, errNoAccount
	}

	// 2. An account an administrator already created, matched by the verified
	//    address. This is the invitation: create the account with no password,
	//    and the first sign-in claims it.
	u, err = s.identity.UserByEmail(ctx, claims.Email)
	if err == nil {
		if u.Disabled {
			return identity.User{}, errAccountDisabled
		}
		if err := s.identity.LinkIdentity(ctx, u.ID, s.oidc.name, claims.Sub, claims.Email); err != nil {
			return identity.User{}, err
		}
		return u, nil
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return identity.User{}, err
	}

	// 3. Nobody here yet. Only an allow-listed domain gets an account made for
	//    it; CILogon vouching for someone is not this deployment vouching.
	if !s.oidc.autoProvisions(claims.Email) {
		return identity.User{}, errNoAccount
	}
	// No password: the account authenticates through the provider, so there is
	// nothing here to change or to leak.
	created, err := s.identity.CreateUser(ctx, claims.Email, claims.Name, s.oidc.defaultRole, "")
	if err != nil {
		return identity.User{}, err
	}
	if err := s.identity.LinkIdentity(ctx, created.ID, s.oidc.name, claims.Sub, claims.Email); err != nil {
		return identity.User{}, err
	}
	log.Printf("api: provisioned %s from %s (allow-listed domain)", created.Email, s.oidc.name)
	return created, nil
}

// safeNextPath keeps only same-origin absolute paths, so a crafted `next`
// cannot turn sign-in into an open redirect. "//host" is rejected because a
// browser reads it as protocol-relative and leaves the site.
func safeNextPath(raw string) string {
	if len(raw) > 0 && raw[0] == '/' && (len(raw) == 1 || raw[1] != '/') {
		return raw
	}
	return ""
}

func (s *Server) oidcCookie(r *http.Request, name, value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // Lax, not Strict: the provider redirects us back
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
	if value == "" || ttl == 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(ttl / time.Second)
	}
	return c
}

func randomState() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
