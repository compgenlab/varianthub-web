package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a user, team or token does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when an email or team name is already taken.
var ErrExists = errors.New("already exists")

// Store holds accounts, teams, tokens, sessions and grants.
type Store struct {
	pool  *pgxpool.Pool
	nowFn func() int64
}

// NewStore wraps an existing pool. The pool is shared with the catalog: these
// are the same database, and a second pool would double the connection budget
// for no benefit.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, nowFn: func() int64 { return time.Now().Unix() }}
}

// SetNow overrides the clock, for tests.
func (s *Store) SetNow(fn func() int64) { s.nowFn = fn }

// The projection carries whether a password exists, never the hash itself: no
// caller of scanUser needs it, and a hash that is never selected cannot be
// logged, serialized or compared by accident.
const userCols = `id, email, name, role, disabled, password_hash = '', created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Disabled, &u.SSO,
		&u.CreatedAt, &u.UpdatedAt)
	return u, err
}

// CreateUser adds an account. An empty password means the account cannot log in
// with one — it authenticates elsewhere, or only via tokens.
func (s *Store) CreateUser(ctx context.Context, email, name, role, password string) (User, error) {
	email = NormalizeEmail(email)
	if email == "" || !strings.Contains(email, "@") {
		return User{}, fmt.Errorf("a user needs an email address")
	}
	switch role {
	case RoleAdmin, RoleMember:
	case "":
		role = RoleMember
	default:
		return User{}, fmt.Errorf("unknown role %q (want %s or %s)", role, RoleAdmin, RoleMember)
	}
	var hash string
	if password != "" {
		h, err := HashPassword(password)
		if err != nil {
			return User{}, err
		}
		hash = h
	}
	now := s.nowFn()
	u := User{
		ID: NewID(), Email: email, Name: name, Role: role,
		// The row is not read back, so the derived flag is set here too. An
		// account created with no password authenticates elsewhere.
		SSO:       hash == "",
		CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO app_user (id,email,name,role,password_hash,disabled,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,false,$6,$6)`,
		u.ID, u.Email, u.Name, u.Role, hash, now)
	if err != nil {
		if strings.Contains(err.Error(), "app_user_email") {
			return User{}, fmt.Errorf("%w: an account for %s", ErrExists, email)
		}
		return User{}, err
	}
	return u, nil
}

// UserByEmail looks an account up for login.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM app_user WHERE lower(email)=lower($1)`, NormalizeEmail(email))
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("user %q: %w", email, ErrNotFound)
	}
	return u, err
}

// User returns one account.
func (s *Store) User(ctx context.Context, id string) (User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM app_user WHERE id=$1`, id)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("user %q: %w", id, ErrNotFound)
	}
	return u, err
}

// ListUsers returns every account.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM app_user ORDER BY lower(email)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers reports how many accounts exist, which is how the server decides
// whether it is looking at a fresh install that still needs bootstrapping.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM app_user`).Scan(&n)
	return n, err
}

// Authenticate checks an email and password, returning the account.
//
// A disabled account never authenticates, and the error does not distinguish an
// unknown address from a wrong password: saying which would let anyone probe
// for registered addresses.
func (s *Store) Authenticate(ctx context.Context, email, password string) (User, error) {
	var u User
	var hash string
	row := s.pool.QueryRow(ctx,
		`SELECT `+userCols+`, password_hash FROM app_user WHERE lower(email)=lower($1)`,
		NormalizeEmail(email))
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Disabled, &u.SSO,
		&u.CreatedAt, &u.UpdatedAt, &hash)
	if err != nil || u.Disabled || !CheckPassword(hash, password) {
		return User{}, errors.New("invalid email or password")
	}
	return u, nil
}

// ErrNoLocalPassword is returned when an account has no password here to change.
var ErrNoLocalPassword = errors.New("this account signs in through an identity provider, so it has no password here")

// ChangePassword replaces an account's own password, given the current one.
//
// The current password is required even though the caller is already
// authenticated. A session cookie or an API token is a bearer credential: if one
// is stolen, being able to set a new password without knowing the old one would
// let the thief lock the owner out of their own account. Re-proving knowledge of
// the password is what makes that a read of the account rather than a takeover.
//
// An SSO account is refused rather than quietly given a local password, which
// would create a second way in that bypasses the identity provider entirely.
func (s *Store) ChangePassword(ctx context.Context, userID, current, next string) error {
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT password_hash FROM app_user WHERE id=$1`, userID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("user %q: %w", userID, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if hash == "" {
		return ErrNoLocalPassword
	}
	if !CheckPassword(hash, current) {
		return errors.New("the current password is incorrect")
	}
	if CheckPassword(hash, next) {
		return errors.New("the new password is the same as the current one")
	}
	return s.SetPassword(ctx, userID, next)
}

// EndOtherSessions logs out every session for an account except one.
//
// Called after a password change: someone changing their password because they
// think it leaked expects that to end whatever the leak was being used for. API
// tokens deliberately survive — they are separate credentials with their own
// revocation, and silently breaking a CI job is not what "change my password"
// asked for.
func (s *Store) EndOtherSessions(ctx context.Context, userID, keepID string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM user_session WHERE user_id=$1 AND id <> $2`, userID, keepID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetPassword replaces an account's password, with no check on the current one.
// This is the administrator's reset path; a user changing their own goes through
// ChangePassword.
func (s *Store) SetPassword(ctx context.Context, userID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE app_user SET password_hash=$2, updated_at=$3 WHERE id=$1`,
		userID, hash, s.nowFn())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %q: %w", userID, ErrNotFound)
	}
	return nil
}

// SetRole changes an account's role.
func (s *Store) SetRole(ctx context.Context, userID, role string) error {
	if role != RoleAdmin && role != RoleMember {
		return fmt.Errorf("unknown role %q", role)
	}
	if role == RoleMember {
		if err := s.guardLastAdmin(ctx, userID); err != nil {
			return err
		}
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE app_user SET role=$2, updated_at=$3 WHERE id=$1`, userID, role, s.nowFn())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %q: %w", userID, ErrNotFound)
	}
	return nil
}

// SetDisabled enables or disables an account. Disabling is preferred to
// deletion: a job's owner should remain resolvable after the person leaves.
func (s *Store) SetDisabled(ctx context.Context, userID string, disabled bool) error {
	if disabled {
		if err := s.guardLastAdmin(ctx, userID); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE app_user SET disabled=$2, updated_at=$3 WHERE id=$1`, userID, disabled, s.nowFn())
	return err
}

// guardLastAdmin refuses a change that would leave no active administrator: an
// installation with none cannot register a source or publish a snapshot again
// without direct database access.
//
// The condition is on the *target* — an account that is not an active
// administrator cannot be the last one, so demoting or disabling an ordinary
// member is always allowed, including on an installation that has no
// administrators at all.
func (s *Store) guardLastAdmin(ctx context.Context, userID string) error {
	var isLast bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app_user WHERE id=$1 AND role=$2 AND NOT disabled)
		   AND NOT EXISTS (SELECT 1 FROM app_user WHERE role=$2 AND NOT disabled AND id <> $1)`,
		userID, RoleAdmin).Scan(&isLast)
	if err != nil {
		return err
	}
	if isLast {
		return errors.New("this is the last administrator; promote another account first")
	}
	return nil
}
