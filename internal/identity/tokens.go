package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionTTL is how long a browser session lasts.
const SessionTTL = 30 * 24 * time.Hour

// Token is a personal API token, as shown in a listing. The secret is absent:
// it exists only in the response to the call that created it.
type Token struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`
	// ExpiresAt is when the token stops working. Zero means never, which is
	// what tokens issued before lifetimes existed carry.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// Active reports whether the token may still authenticate at the given time.
//
// Takes the time rather than reading the clock so a listing can render the same
// answer the authentication path would give, without the two drifting.
func (t Token) Active(now int64) bool {
	if t.RevokedAt != 0 {
		return false
	}
	return t.ExpiresAt == 0 || now < t.ExpiresAt
}

// Expired reports whether the token lapsed rather than being revoked. The two
// are different things to tell somebody looking at a list.
func (t Token) Expired(now int64) bool {
	return t.RevokedAt == 0 && t.ExpiresAt != 0 && now >= t.ExpiresAt
}

// CreateToken mints a personal API token, returning the secret to show once.
// TokenLifetimes are the lifetimes a token may be issued for, in days.
//
// A fixed set rather than a free number: the choice is "how long do I need
// this", which has a handful of real answers, and an open field invites both
// a typo and a decade.
var TokenLifetimes = []int{1, 14, 30, 90, 180, 365}

// ValidLifetime reports whether days is one of the offered lifetimes.
func ValidLifetime(days int) bool {
	for _, d := range TokenLifetimes {
		if d == days {
			return true
		}
	}
	return false
}

// CreateToken issues a token valid for the given number of days.
//
// The lifetime is checked here rather than only in the handler, because this is
// the one path every caller goes through — the API, the explorer, and the tests.
func (s *Store) CreateToken(ctx context.Context, userID, name string, days int) (Token, string, error) {
	if !ValidLifetime(days) {
		return Token{}, "", fmt.Errorf("token lifetime %d is not one of %v days", days, TokenLifetimes)
	}
	secret, prefix, hash, err := NewToken()
	if err != nil {
		return Token{}, "", err
	}
	now := s.nowFn()
	t := Token{
		ID: NewID(), Name: strings.TrimSpace(name), Prefix: prefix,
		CreatedAt: now,
		ExpiresAt: now + int64(days)*24*60*60,
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO api_token (id,user_id,name,prefix,hash,created_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		t.ID, userID, t.Name, t.Prefix, hash, t.CreatedAt, t.ExpiresAt)
	if err != nil {
		return Token{}, "", err
	}
	return t, secret, nil
}

// ListTokens returns a user's tokens, revoked ones included so a rotation is
// visible rather than the old token simply vanishing.
func (s *Store) ListTokens(ctx context.Context, userID string) ([]Token, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,name,prefix,created_at,last_used_at,revoked_at,expires_at
		  FROM api_token WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.CreatedAt,
			&t.LastUsedAt, &t.RevokedAt, &t.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken disables a token. It is not deleted: the row is the record that
// the token existed and when it stopped working, which is what an audit of a
// leak needs.
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_token SET revoked_at=$3 WHERE id=$1 AND user_id=$2 AND revoked_at=0`,
		tokenID, userID, s.nowFn())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("token %q: %w", tokenID, ErrNotFound)
	}
	return nil
}

// UserByToken resolves a presented token to its account.
//
// The prefix locates the row and the hash is then compared in constant time —
// the prefix alone never authenticates anything. A revoked token and a disabled
// account both fail.
func (s *Store) UserByToken(ctx context.Context, secret string) (User, error) {
	prefix, ok := PrefixOf(secret)
	if !ok {
		return User{}, ErrNotFound
	}
	var (
		u       User
		hash    string
		revoked int64
		expires int64
		tokenID string
	)
	row := s.pool.QueryRow(ctx, `
		SELECT t.id, t.hash, t.revoked_at, t.expires_at,
		       u.id,u.email,u.name,u.role,u.disabled,u.password_hash = '',
		       u.created_at,u.updated_at
		  FROM api_token t JOIN app_user u ON u.id = t.user_id
		 WHERE t.prefix=$1`, prefix)
	if err := row.Scan(&tokenID, &hash, &revoked, &expires, &u.ID, &u.Email, &u.Name,
		&u.Role, &u.Disabled, &u.SSO, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, ErrNotFound
	}
	// Expiry is enforced here, where the credential is checked, rather than by
	// a sweep. A sweep that has not run yet would leave a lapsed token working,
	// which is the one thing a deadline is supposed to prevent. Zero means a
	// token issued before lifetimes existed, which never lapses.
	if expires != 0 && s.nowFn() >= expires {
		return User{}, ErrNotFound
	}
	if revoked != 0 || u.Disabled || !TokenMatches(hash, secret) {
		return User{}, ErrNotFound
	}
	// Best effort: a failed touch must not fail the request it authenticated.
	_, _ = s.pool.Exec(ctx, `UPDATE api_token SET last_used_at=$2 WHERE id=$1`, tokenID, s.nowFn())
	return u, nil
}

// --- browser sessions ---

// CreateSession issues a session for a logged-in user.
func (s *Store) CreateSession(ctx context.Context, userID string) (string, int64, error) {
	id := NewID() + NewID() // 256 bits: a session id is a bearer credential
	now := s.nowFn()
	exp := now + int64(SessionTTL/time.Second)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_session (id,user_id,created_at,expires_at) VALUES ($1,$2,$3,$4)`,
		id, userID, now, exp)
	return id, exp, err
}

// UserBySession resolves a session cookie to its account.
func (s *Store) UserBySession(ctx context.Context, id string) (User, error) {
	var u User
	row := s.pool.QueryRow(ctx, `
		SELECT u.id,u.email,u.name,u.role,u.disabled,u.password_hash = '',
		       u.created_at,u.updated_at
		  FROM user_session s JOIN app_user u ON u.id = s.user_id
		 WHERE s.id=$1 AND s.expires_at > $2`, id, s.nowFn())
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Disabled, &u.SSO,
		&u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, ErrNotFound
	}
	if u.Disabled {
		return User{}, ErrNotFound
	}
	return u, nil
}

// EndSession logs a session out.
func (s *Store) EndSession(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_session WHERE id=$1`, id)
	return err
}

// PurgeExpiredSessions removes sessions past their expiry.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM user_session WHERE expires_at <= $1`, s.nowFn())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Resolve builds the Caller for a request's credentials.
//
// Order matters only in that a personal token is checked before a session: a
// request carrying both is a script acting as itself, not a browser.
func (s *Store) Resolve(ctx context.Context, bearer, sessionID string) (Caller, error) {
	// The bootstrap credential is checked first and separately: it belongs to no
	// account, so it can never be resolved through the token or session tables.
	if strings.HasPrefix(bearer, BootstrapPrefix) {
		ok, err := s.CheckBootstrap(ctx, bearer)
		if err != nil {
			return Caller{}, err
		}
		if !ok {
			return Caller{}, nil
		}
		return Caller{Bootstrap: true}, nil
	}

	var u User
	var err error
	viaToken := false
	switch {
	case strings.HasPrefix(bearer, TokenPrefix):
		u, err = s.UserByToken(ctx, bearer)
		viaToken = true
	case sessionID != "":
		u, err = s.UserBySession(ctx, sessionID)
	default:
		return Caller{}, nil
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Caller{}, nil // an unrecognised credential is anonymous, not an error
		}
		return Caller{}, err
	}
	teams, err := s.TeamIDsFor(ctx, u.ID)
	if err != nil {
		return Caller{}, err
	}
	return Caller{User: &u, TeamIDs: teams, ViaToken: viaToken}, nil
}
