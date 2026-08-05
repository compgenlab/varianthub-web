package identity

import (
	"context"
	"errors"
	"fmt"
)

// BootstrapPrefix marks a bootstrap token, so it is never mistaken for a
// personal one and is as greppable as the rest.
const BootstrapPrefix = "cgl_vhb_"

// ErrNoBootstrap is returned when no usable bootstrap token exists — either one
// was never issued, or it has already been used.
var ErrNoBootstrap = errors.New("no bootstrap token")

// NeedsBootstrap reports whether the installation has no administrator and so
// cannot yet be administered by anyone.
//
// A disabled administrator does not count: an account nobody can sign in to
// cannot create the next one either.
func (s *Store) NeedsBootstrap(ctx context.Context) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM app_user WHERE role=$1 AND NOT disabled`, RoleAdmin).Scan(&n)
	return n == 0, err
}

// IssueBootstrap mints a bootstrap token, replacing any unconsumed one.
//
// Replacing rather than reusing means a restart invalidates a token that was
// printed to a log and never used, which keeps the window in which that log is
// dangerous as short as the process lifetime.
func (s *Store) IssueBootstrap(ctx context.Context) (string, error) {
	secret, _, hash, err := newSecret(BootstrapPrefix)
	if err != nil {
		return "", err
	}
	prefix, _ := PrefixOf(secret)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM bootstrap_token WHERE consumed_at = 0`); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO bootstrap_token (id,prefix,hash,created_at) VALUES ($1,$2,$3,$4)`,
		NewID(), prefix, hash, s.nowFn()); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return secret, nil
}

// CheckBootstrap reports whether a presented secret is the live bootstrap token.
//
// It authenticates only while the installation still has no administrator: once
// one exists the circle is broken and the token has no further purpose, so it
// stops working whether or not it was formally consumed.
func (s *Store) CheckBootstrap(ctx context.Context, secret string) (bool, error) {
	prefix, ok := PrefixOf(secret)
	if !ok || !hasPrefix(secret, BootstrapPrefix) {
		return false, nil
	}
	needs, err := s.NeedsBootstrap(ctx)
	if err != nil || !needs {
		return false, err
	}
	var hash string
	err = s.pool.QueryRow(ctx,
		`SELECT hash FROM bootstrap_token WHERE prefix=$1 AND consumed_at=0`, prefix).Scan(&hash)
	if err != nil {
		return false, nil // no such token, or already consumed
	}
	return TokenMatches(hash, secret), nil
}

// ConsumeBootstrap marks the bootstrap token used.
func (s *Store) ConsumeBootstrap(ctx context.Context, secret string) error {
	prefix, ok := PrefixOf(secret)
	if !ok {
		return ErrNoBootstrap
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE bootstrap_token SET consumed_at=$2 WHERE prefix=$1 AND consumed_at=0`,
		prefix, s.nowFn())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: already used", ErrNoBootstrap)
	}
	return nil
}
