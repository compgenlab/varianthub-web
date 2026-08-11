package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Access request states.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDeclined = "declined"
)

// AccessRequest is somebody who authenticated and has no account here yet.
//
// Everything on it came from the identity provider. Nothing is self-asserted,
// which is what makes approving one safe: an administrator is agreeing that a
// verified address should have an account, not taking somebody's word for who
// they are.
type AccessRequest struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Provider string `json:"provider"`
	// Subject is the provider's own identifier. Not published: it is an opaque
	// key, and showing it invites treating it as an address.
	Subject    string `json:"-"`
	Status     string `json:"status"`
	DecidedBy  string `json:"decided_by,omitempty"`
	DecidedAt  int64  `json:"decided_at,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
}

const requestCols = `id, email, name, provider, subject, status,
	COALESCE(decided_by,''), decided_at, created_at, last_seen_at`

func scanRequest(row interface{ Scan(...any) error }) (AccessRequest, error) {
	var a AccessRequest
	err := row.Scan(&a.ID, &a.Email, &a.Name, &a.Provider, &a.Subject, &a.Status,
		&a.DecidedBy, &a.DecidedAt, &a.CreatedAt, &a.LastSeenAt)
	return a, err
}

// RecordAccessRequest notes that this identity asked for access, or that it has
// asked again.
//
// Idempotent on (provider, subject): somebody who tries once a day for a week
// is one request that has been seen seven times, not seven requests. Only
// last_seen_at moves, so the queue keeps its original order and a persistent
// visitor cannot push themselves to the front of it.
//
// A previously declined request stays declined. Reopening it on the next
// sign-in would make declining useless — the person would reappear in the queue
// the moment they tried again, and an administrator would decline the same
// person forever.
func (s *Store) RecordAccessRequest(ctx context.Context, provider, subject, email, name string) (AccessRequest, error) {
	email = NormalizeEmail(email)
	if email == "" || provider == "" || subject == "" {
		return AccessRequest{}, errors.New("an access request needs a provider identity and a verified address")
	}
	id, now := NewID(), s.nowFn()
	row := s.pool.QueryRow(ctx, `
		INSERT INTO access_request (id,email,name,provider,subject,status,created_at,last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
		ON CONFLICT (provider,subject) DO UPDATE
		   SET last_seen_at = excluded.last_seen_at,
		       -- The address and name are refreshed because the provider is
		       -- authoritative for both and either can change between attempts.
		       email = excluded.email,
		       name  = excluded.name
		RETURNING `+requestCols,
		id, email, name, provider, subject, RequestPending, now)
	return scanRequest(row)
}

// ListAccessRequests returns requests, pending first and oldest first within
// each state — the order they should be worked through.
func (s *Store) ListAccessRequests(ctx context.Context) ([]AccessRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+requestCols+`
		  FROM access_request
		 ORDER BY (status <> 'pending'), created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessRequest{}
	for rows.Next() {
		a, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ApproveAccessRequest creates the account the request asked for and links the
// identity to it, so the next sign-in is an ordinary one.
//
// The identity is linked here rather than left for that sign-in to do, because
// the address is the weaker key: somebody whose institution reassigns their
// address would otherwise arrive as a stranger. Linking now pins the account to
// the subject that was actually verified.
//
// One transaction. An account created without its link is an invitation that
// only works while the address holds; a link without an account is a row
// pointing at nothing.
func (s *Store) ApproveAccessRequest(ctx context.Context, id, decidedBy, role string) (User, error) {
	if role == "" {
		role = RoleMember
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	req, err := scanRequest(tx.QueryRow(ctx,
		`SELECT `+requestCols+` FROM access_request WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return User{}, fmt.Errorf("access request %s: %w", id, ErrNotFound)
	}
	if req.Status == RequestApproved {
		// Already done. Report the account rather than failing: the caller's
		// intent is satisfied, and a double-click should not be an error.
		u, err := s.userByEmail(ctx, tx, req.Email)
		if err != nil {
			return User{}, err
		}
		return u, tx.Commit(ctx)
	}

	now, uid := s.nowFn(), NewID()
	// No password: the account authenticates through the provider, so there is
	// nothing stored here to change or to leak.
	//
	// The conflict target is the address, not the id — the unique index is on
	// lower(email), and an id collision is not the case that happens. The one
	// that does is an administrator adding somebody by hand while their request
	// sits pending: approving it should then link the identity to the account
	// that already exists rather than failing on a duplicate.
	if _, err := tx.Exec(ctx, `
		INSERT INTO app_user (id,email,name,role,tier,created_at,updated_at)
		VALUES ($1,$2,$3,$4,'standard',$5,$5)
		ON CONFLICT (lower(email)) DO NOTHING`,
		uid, req.Email, req.Name, role, now); err != nil {
		return User{}, err
	}
	u, err := s.userByEmail(ctx, tx, req.Email)
	if err != nil {
		return User{}, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO user_identity (id,user_id,provider,subject,email,created_at,last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,0)
		ON CONFLICT (provider,subject) DO NOTHING`,
		NewID(), u.ID, req.Provider, req.Subject, req.Email, now); err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE access_request SET status=$2, decided_by=NULLIF($3,''), decided_at=$4 WHERE id=$1`,
		id, RequestApproved, decidedBy, now); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return u, nil
}

// DeclineAccessRequest records a decision not to grant access.
//
// The row stays. A declined request that vanishes is one the same person raises
// again next week with nothing to say it was already considered.
func (s *Store) DeclineAccessRequest(ctx context.Context, id, decidedBy string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE access_request SET status=$2, decided_by=NULLIF($3,''), decided_at=$4
		 WHERE id=$1`, id, RequestDeclined, decidedBy, s.nowFn())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("access request %s: %w", id, ErrNotFound)
	}
	return nil
}

// userByEmail reads an account through whatever is running the statement — the
// pool, or a transaction that has not committed yet.
func (s *Store) userByEmail(ctx context.Context, q pgxQuerier, email string) (User, error) {
	return scanUser(q.QueryRow(ctx,
		`SELECT `+userCols+` FROM app_user WHERE lower(email)=lower($1)`,
		strings.TrimSpace(email)))
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
