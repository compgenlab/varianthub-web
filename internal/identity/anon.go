package identity

import (
	"context"
	"time"
)

// AnonTTL is how long an idle anonymous session lasts.
//
// Long, because it is what scopes "my results" for someone who never signed in:
// the whole point of keeping a history is that yesterday's run is still there
// tomorrow. It is not a credential for anything but that.
const AnonTTL = 90 * 24 * time.Hour

// CreateAnonSession issues a session for a visitor who has not signed in.
//
// 256 bits, like an authenticated session: it is a bearer value, and while it
// authorizes nothing it does scope one visitor's job history. Guessing one
// should not be a way to read somebody's results.
func (s *Store) CreateAnonSession(ctx context.Context) (string, error) {
	id := NewID() + NewID()
	now := s.nowFn()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO anon_session (id, created_at, seen_at) VALUES ($1,$2,$2)`,
		id, now); err != nil {
		return "", err
	}
	return id, nil
}

// AnonSession reports whether an id is one this server issued, refreshing it.
//
// The check is the entire point: an id the client invented must not work, or
// this is vh_history again under a new name.
func (s *Store) AnonSession(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	now := s.nowFn()
	cutoff := now - int64(AnonTTL/time.Second)
	tag, err := s.pool.Exec(ctx,
		`UPDATE anon_session SET seen_at=$2 WHERE id=$1 AND seen_at >= $3`,
		id, now, cutoff)
	if err != nil {
		return false, err
	}
	// One statement rather than a read then a write: it both proves the session
	// exists and records the activity, so a concurrent request cannot see it
	// expire between the two.
	return tag.RowsAffected() == 1, nil
}

// PurgeAnonSessions drops sessions idle past the TTL, returning how many went.
func (s *Store) PurgeAnonSessions(ctx context.Context) (int64, error) {
	cutoff := s.nowFn() - int64(AnonTTL/time.Second)
	tag, err := s.pool.Exec(ctx, `DELETE FROM anon_session WHERE seen_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
