package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/compgenlab/varianthub-web/internal/identity"
)

// SessionCookie is the browser's session credential.
//
// Distinct from the existing varhub_session, which is a self-asserted history
// scope with no authentication behind it: an anonymous visitor still gets one,
// and it must not be mistaken for proof of who they are.
const SessionCookie = "vh_auth"

type callerKey struct{}

// callerOf returns the identity resolved for this request. An unauthenticated
// request yields the zero Caller, which is anonymous — handlers never have to
// distinguish "no credential" from "not looked up yet".
func callerOf(r *http.Request) identity.Caller {
	c, _ := r.Context().Value(callerKey{}).(identity.Caller)
	return c
}

// withCaller resolves credentials once per request and attaches the result.
//
// Every authorization decision downstream reads this one value, so a route
// cannot accidentally authenticate differently from its neighbour.
func (s *Server) withCaller(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := s.resolveCaller(r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not resolve identity")
			log.Printf("api: resolving identity: %v", err)
			return
		}
		ctx := context.WithValue(r.Context(), callerKey{}, caller)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerOf extracts the token from an "Authorization: Bearer <token>" header.
func bearerOf(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

func (s *Server) resolveCaller(r *http.Request) (identity.Caller, error) {
	bearer := bearerOf(r)
	// An Authorization header in some other scheme is not ours to interpret, so
	// the caller is anonymous rather than rejected — a client sending a Basic
	// header to an anonymous-allowed instance should still be served.
	if bearer != "" && !identity.IsCredential(bearer) {
		return identity.Caller{}, nil
	}
	if s.identity == nil {
		return identity.Caller{}, nil
	}
	var session string
	if c, err := r.Cookie(SessionCookie); err == nil {
		session = strings.TrimSpace(c.Value)
	}
	return s.identity.Resolve(r.Context(), bearer, session)
}

// requireAuth rejects an unidentified caller unless the deployment opted into
// anonymous access with VHW_ALLOW_ANONYMOUS.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AllowAnonymous || !callerOf(r).Anonymous() {
			h.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "sign in to continue")
	})
}

// requireAdmin gates the administration routes.
func (s *Server) requireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		if c.IsAdmin() {
			h.ServeHTTP(w, r)
			return
		}
		// 401 when we do not know who is asking, 403 when we do: the first is
		// fixed by signing in and the second never is, and telling them apart
		// saves an operator guessing which.
		if c.Anonymous() {
			writeError(w, http.StatusUnauthorized, "sign in to continue")
			return
		}
		writeError(w, http.StatusForbidden, "this action needs an administrator account")
	})
}

// --- session endpoints ---

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.identity == nil {
		writeError(w, http.StatusServiceUnavailable, "accounts unavailable")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	u, err := s.identity.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		// One message for a wrong password and an unknown address alike: the
		// difference between them is a list of who has an account here.
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	id, exp, err := s.identity.CreateSession(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	http.SetCookie(w, s.sessionCookie(r, id, time.Unix(exp, 0)))
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil && s.identity != nil {
		if err := s.identity.EndSession(r.Context(), c.Value); err != nil {
			log.Printf("api: ending session: %v", err)
		}
	}
	// Expire the cookie whether or not the session existed, so a stale cookie
	// cannot survive a logout that raced with its expiry.
	http.SetCookie(w, s.sessionCookie(r, "", time.Unix(0, 0)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sessionCookie(r *http.Request, value string, expires time.Time) *http.Cookie {
	c := &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true, // a session cookie script can read is a session cookie script can steal
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		// Secure would make the cookie unusable over plain http, which is how
		// the development stack runs. Set it whenever the request arrived over
		// TLS, so a real deployment gets it without a flag to forget.
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
	if value == "" {
		c.MaxAge = -1
	}
	return c
}

// handleMe describes the current caller, which is what the UI decides what to
// render from.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	out := map[string]any{
		"anonymous": c.Anonymous(),
		"admin":     c.IsAdmin(),
		"label":     c.Label(),
		"bootstrap": c.Bootstrap,
		"teams":     c.TeamIDs,
		// Whether a password can be changed here at all. An SSO account has no
		// password of ours, so the UI shows no form rather than one that fails.
		"can_change_password": c.User != nil && c.User.CanChangePassword(),
		// Whether to offer the institutional sign-in button at all.
		"sso_enabled": s.oidc != nil,
		// Whether an unidentified caller may use the annotation flow. The UI
		// needs this to decide between showing the app and showing a login
		// wall — without it the two disagree, and a server that permits
		// anonymous use still looks closed from the browser.
		"allow_anonymous": s.cfg.AllowAnonymous,
	}
	if c.User != nil {
		out["user"] = c.User
	}
	// Whether the installation still needs its first administrator is not a
	// secret — the login screen has to know, or it cannot tell a new operator
	// what to do next.
	if s.identity != nil {
		if needs, err := s.identity.NeedsBootstrap(r.Context()); err == nil {
			out["needs_bootstrap"] = needs
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleChangePassword lets a caller change their own password.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.User == nil {
		writeError(w, http.StatusForbidden, "a password belongs to an account; sign in first")
		return
	}
	// Checked here as well as in the store so the UI gets the same answer from
	// /auth/me that it would get from submitting: the form is hidden for an SSO
	// account, and hiding it must not be the only thing stopping it.
	if c.User.SSO {
		writeError(w, http.StatusConflict, identity.ErrNoLocalPassword.Error())
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	err := s.identity.ChangePassword(r.Context(), c.User.ID, req.CurrentPassword, req.NewPassword)
	switch {
	case errors.Is(err, identity.ErrNoLocalPassword):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, identity.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such account")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Keep this session alive and end the rest: changing a password should end
	// whatever a leaked one was being used for, without signing the person out
	// of the browser they just did it from.
	var keep string
	if ck, err := r.Cookie(SessionCookie); err == nil {
		keep = ck.Value
	}
	if n, err := s.identity.EndOtherSessions(r.Context(), c.User.ID, keep); err != nil {
		log.Printf("api: ending other sessions for %s: %v", c.User.ID, err)
	} else if n > 0 {
		log.Printf("api: password changed for %s; ended %d other session(s)", c.User.Email, n)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListIdentities shows the external logins linked to the caller's
// account, so someone can see how they are actually signing in.
func (s *Server) handleListIdentities(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.User == nil {
		writeError(w, http.StatusForbidden, "sign in first")
		return
	}
	ids, err := s.identity.Identities(r.Context(), c.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": ids})
}

// --- personal API tokens ---

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.User == nil {
		writeError(w, http.StatusForbidden, "API tokens belong to an account; sign in first")
		return
	}
	toks, err := s.identity.ListTokens(r.Context(), c.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": toks})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.User == nil {
		writeError(w, http.StatusForbidden, "API tokens belong to an account; sign in first")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)

	tok, secret, err := s.identity.CreateToken(r.Context(), c.User.ID, req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The only time the secret exists outside the caller's hands. It carries
	// the owner's role as it is at each use, so an administrator's token
	// administers and stops doing so the moment they are demoted.
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "secret": secret})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.User == nil {
		writeError(w, http.StatusForbidden, "API tokens belong to an account; sign in first")
		return
	}
	err := s.identity.RevokeToken(r.Context(), c.User.ID, r.PathValue("id"))
	if errors.Is(err, identity.ErrNotFound) {
		// Scoped to the caller's own tokens, so this is equally "no such token"
		// and "not yours" — and it must not distinguish them.
		writeError(w, http.StatusNotFound, "no such token")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
