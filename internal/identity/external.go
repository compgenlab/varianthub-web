package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ProviderCILogon is the key stored for a CILogon identity.
const ProviderCILogon = "cilogon"

// Identity is a link between an account and an external provider.
type Identity struct {
	Provider   string `json:"provider"`
	Subject    string `json:"subject"`
	Email      string `json:"email,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at,omitempty"`
}

// UserByIdentity resolves a provider subject to an account.
//
// The subject is the join key rather than the email: a provider's `sub` is
// stable across a name or address change, so someone who changes institution
// email still returns to the same account.
func (s *Store) UserByIdentity(ctx context.Context, provider, subject string) (User, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+prefixed(userCols, "u")+`
		  FROM user_identity i JOIN app_user u ON u.id = i.user_id
		 WHERE i.provider=$1 AND i.subject=$2`, provider, subject)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("identity %s/%s: %w", provider, subject, ErrNotFound)
	}
	return u, err
}

// prefixed qualifies a comma-separated column list with a table alias, so the
// one definition of what a user projection is stays in one place even where a
// join makes the bare names ambiguous.
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = " " + alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

// LinkIdentity attaches a provider identity to an account.
//
// Re-linking the same subject to the same account refreshes the reported email
// and the last-seen time; linking it to a *different* account fails on the
// unique constraint, which is the intent — one external identity is one person.
func (s *Store) LinkIdentity(ctx context.Context, userID, provider, subject, email string) error {
	now := s.nowFn()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_identity (id,user_id,provider,subject,email,created_at,last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$6)
		ON CONFLICT (provider,subject) DO UPDATE
		   SET email = excluded.email, last_seen_at = excluded.last_seen_at
		 WHERE user_identity.user_id = excluded.user_id`,
		NewID(), userID, provider, subject, NormalizeEmail(email), now)
	return err
}

// TouchIdentity records a successful sign-in.
func (s *Store) TouchIdentity(ctx context.Context, provider, subject string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE user_identity SET last_seen_at=$3 WHERE provider=$1 AND subject=$2`,
		provider, subject, s.nowFn())
	return err
}

// Identities lists an account's external logins.
func (s *Store) Identities(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT provider,subject,email,created_at,last_seen_at
		  FROM user_identity WHERE user_id=$1 ORDER BY provider`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.Provider, &i.Subject, &i.Email,
			&i.CreatedAt, &i.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// UnlinkIdentity removes an external login from an account.
//
// Refused when it is the account's only way in — an account with no password and
// no identity cannot sign in at all, and creating one by accident is a support
// ticket rather than a security improvement.
func (s *Store) UnlinkIdentity(ctx context.Context, userID, provider, subject string) error {
	var hasPassword bool
	var identities int
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT password_hash <> '' FROM app_user WHERE id=$1),
		       (SELECT count(*) FROM user_identity WHERE user_id=$1)`, userID).
		Scan(&hasPassword, &identities)
	if err != nil {
		return err
	}
	if !hasPassword && identities <= 1 {
		return errors.New("this is the only way to sign in to this account; set a password first")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM user_identity WHERE user_id=$1 AND provider=$2 AND subject=$3`,
		userID, provider, subject)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("identity %s: %w", provider, ErrNotFound)
	}
	return nil
}
