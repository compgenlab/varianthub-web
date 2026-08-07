// Package identity models who is making a request: accounts, the teams they
// belong to, personal API tokens, and browser sessions.
//
// This is the whole of authentication. There was a deployment-wide HMAC master
// key; it is gone. A shared secret cannot be attributed, revoked individually, or
// told apart from a copy of itself, so scripts now hold personal API tokens
// belonging to an account like everyone else. The one credential that is not a
// person is the bootstrap token, which exists only until the first administrator
// account does — see bootstrap.go.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Roles a user may hold.
const (
	RoleAdmin  = "admin"  // administers the catalog
	RoleMember = "member" // annotates
)

// Team roles.
const (
	TeamOwner  = "owner" // may change membership
	TeamMember = "member"
)

// TokenPrefix marks a personal API token.
//
// Kept in the clear and indexed so a presented token can be located without
// reversing its hash, and chosen to be distinctive so a leaked one is greppable
// by secret scanners.
const TokenPrefix = "cgl_vh_"

// User is an account.
type User struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled,omitempty"`
	// SSO means no password is stored here: the account authenticates through
	// an identity provider. Such an account has no password to change, and
	// saying so is the difference between a form that is absent and one that
	// fails when submitted.
	SSO bool `json:"sso,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// CanChangePassword reports whether this account holds a password we could
// change. False for an SSO account, where the password lives elsewhere.
func (u User) CanChangePassword() bool { return !u.SSO }

// IsAdmin reports whether the user administers the catalog.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// Team is a group that grants can be made to.
type Team struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

// Caller is the resolved identity of one request.
//
// A request is authenticated as a user, as the one-time bootstrap credential, or
// not at all. Kept as one type so every handler asks it the same questions —
// Anonymous, IsAdmin, UserID — instead of branching on which credential happened
// to be presented.
type Caller struct {
	User    *User    // nil when not a user
	TeamIDs []string // teams the user belongs to
	// Bootstrap means the one-time credential that exists only until the first
	// administrator account does. It administers, because creating that account
	// is administration; it stops working the moment the account exists.
	Bootstrap bool
	// ViaToken records that a personal API token authenticated this request,
	// rather than a browser session.
	//
	// Not an authorization question — a token carries exactly its owner's
	// rights. It decides which *surface* answers: the web app needs paginated
	// tables, account management and the whole administration area, and none of
	// that has to exist for a program driving annotations from a script. A
	// smaller published surface is a smaller thing to keep working.
	ViaToken bool
	// AnonSession is a server-issued session for a visitor who has not signed
	// in. It identifies nobody; it scopes which results are theirs.
	//
	// Its presence is what separates an anonymous visitor from an
	// unauthenticated request. Before it existed the two were the same thing,
	// so a bare curl could submit against the published API by looking exactly
	// like somebody browsing the site.
	AnonSession string
}

// External reports whether the request should see only the published REST API.
//
// A token means a program: somebody outside the web app, driving annotations
// from a script. That is the only thing this asks.
//
// Deliberately not "has no session". Presenting nothing is a different problem
// with a different answer — Credentialed reports that, and the routes require
// it separately. Folding the two together made an expired session look like a
// missing endpoint instead of a signed-out one, and 404'd the bootstrap flow,
// which creates the first administrator with a bearer credential precisely
// because no session can exist yet.
func (c Caller) External() bool { return c.ViaToken }

// Credentialed reports whether the request presented anything at all: an
// account, a token, the bootstrap credential, or a session this server issued
// to an anonymous visitor.
//
// The last is what makes anonymous browsing distinguishable from an
// unauthenticated request. While anonymity was a header the client wrote for
// itself the two were the same, which is how a bare curl could submit against
// the published API by claiming to be somebody browsing the site.
func (c Caller) Credentialed() bool { return !c.Anonymous() || c.AnonSession != "" }

// Scope identifies whose results these are for a caller with no account.
func (c Caller) Scope() string { return c.AnonSession }

// Anonymous reports whether the request carried no credential at all.
func (c Caller) Anonymous() bool { return c.User == nil && !c.Bootstrap }

// IsAdmin reports whether the caller may administer the catalog.
//
// Administration is a property of an account, so a token administers only
// because the person who owns it does — demote them and every token they hold
// stops administering, with nothing to revoke separately.
func (c Caller) IsAdmin() bool {
	if c.Bootstrap {
		return true
	}
	return c.User != nil && c.User.IsAdmin()
}

// Label identifies the caller in logs and job ownership.
func (c Caller) Label() string {
	switch {
	case c.User != nil:
		return c.User.Email
	case c.Bootstrap:
		return "bootstrap"
	default:
		return "anonymous"
	}
}

// UserID returns the owning user's id, or "" for a non-user caller.
func (c Caller) UserID() string {
	if c.User == nil {
		return ""
	}
	return c.User.ID
}

// InTeam reports membership.
func (c Caller) InTeam(teamID string) bool {
	for _, t := range c.TeamIDs {
		if t == teamID {
			return true
		}
	}
	return false
}

// --- credentials ---

// HashPassword returns a bcrypt hash.
func HashPassword(plain string) (string, error) {
	if len(plain) < 8 {
		return "", fmt.Errorf("a password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(h), err
}

// CheckPassword reports whether a plaintext password matches a stored hash.
//
// An account with no password — one that authenticates elsewhere — never
// matches, rather than matching the empty string.
func CheckPassword(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// NewToken mints a personal API token, returning the secret to show once and
// the prefix and hash to store.
//
// The secret is never recoverable afterwards. That is the point: a database
// leak yields hashes, and a hash cannot be presented as a credential.
func NewToken() (secret, prefix, hash string, err error) {
	return newSecret(TokenPrefix)
}

// newSecret mints a random credential under a given marker prefix. The markers
// are disjoint, so which kind of credential was presented is decidable from the
// string alone — a personal token is never tried against the bootstrap table or
// the other way round.
func newSecret(marker string) (secret, prefix, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	body := base64.RawURLEncoding.EncodeToString(b[:])
	secret = marker + body
	// Enough of the body to be unique without being enough to guess the rest.
	prefix = marker + body[:8]
	return secret, prefix, HashToken(secret), nil
}

// HashToken hashes a token secret for storage and lookup.
//
// SHA-256 rather than bcrypt: a token is 256 bits of entropy from a CSPRNG, so
// it is not guessable and needs no work factor, and every request presents one —
// a deliberately slow hash would put bcrypt's cost on the hot path.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// PrefixOf extracts the stored prefix from a presented token, so it can be
// looked up before the hash is compared. It accepts either credential marker;
// the marker is part of the prefix, so the lookup cannot cross between them.
func PrefixOf(secret string) (string, bool) {
	// Longest marker first: the markers are disjoint, but testing the shorter
	// one first would still be a trap waiting for the next marker added.
	for _, marker := range []string{BootstrapPrefix, TokenPrefix} {
		if !strings.HasPrefix(secret, marker) {
			continue
		}
		body := secret[len(marker):]
		if len(body) < 8 {
			return "", false
		}
		return marker + body[:8], true
	}
	return "", false
}

// IsCredential reports whether a bearer value is one of ours at all, so an
// unrelated Authorization scheme is treated as anonymous rather than as a failed
// login attempt.
func IsCredential(s string) bool {
	return strings.HasPrefix(s, TokenPrefix) || strings.HasPrefix(s, BootstrapPrefix)
}

func hasPrefix(s, prefix string) bool { return strings.HasPrefix(s, prefix) }

// TokenMatches compares a presented token against a stored hash in constant
// time, so a caller cannot learn the hash by timing repeated attempts.
func TokenMatches(storedHash, secret string) bool {
	got := HashToken(secret)
	return subtle.ConstantTimeCompare([]byte(storedHash), []byte(got)) == 1
}

// NewID returns a random identifier for a user, team, token or session.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("identity: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// NormalizeEmail lowercases and trims an address, matching the unique index.
func NormalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
