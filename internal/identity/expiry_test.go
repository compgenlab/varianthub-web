package identity

import (
	"context"
	"errors"
	"testing"
)

// A lapsed token stops authenticating the moment it lapses, checked where the
// credential is verified.
//
// Not by a sweep: a sweep that has not run yet leaves an expired token working,
// which is the one thing a deadline exists to prevent. The clock is moved rather
// than the row edited, so this exercises the same comparison a real request does.
func TestExpiredTokenStopsAuthenticating(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	now := int64(1_000_000)
	s.SetNow(func() int64 { return now })

	u, err := s.CreateUser(ctx, "a@example.com", "A", RoleMember, "password123")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := s.CreateToken(ctx, u.ID, "one day", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ExpiresAt != now+86400 {
		t.Errorf("ExpiresAt = %d, want %d", tok.ExpiresAt, now+86400)
	}

	if _, err := s.UserByToken(ctx, secret); err != nil {
		t.Fatalf("a fresh token failed: %v", err)
	}

	// One second before the deadline it still works...
	now = tok.ExpiresAt - 1
	if _, err := s.UserByToken(ctx, secret); err != nil {
		t.Errorf("token failed before its deadline: %v", err)
	}
	// ...and at it, it does not. The boundary is the interesting part: an
	// off-by-one here is a day of unintended validity.
	now = tok.ExpiresAt
	if _, err := s.UserByToken(ctx, secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("token still authenticated at its deadline: %v", err)
	}
	now = tok.ExpiresAt + 86400
	if _, err := s.UserByToken(ctx, secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("token still authenticated a day past its deadline: %v", err)
	}

	// The listing distinguishes lapsed from revoked; they are different things
	// to tell somebody looking at the list, and only one of them was a decision.
	list, err := s.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d tokens", len(list))
	}
	if !list[0].Expired(now) {
		t.Error("a lapsed token does not report expired")
	}
	if list[0].Active(now) {
		t.Error("a lapsed token reports active")
	}
	if list[0].RevokedAt != 0 {
		t.Error("a lapsed token reports as revoked")
	}
}

// Only the offered lifetimes are accepted, and the check is in the store rather
// than only the handler — the explorer and the tests go through here too.
func TestTokenLifetimeIsRestricted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "b@example.com", "B", RoleMember, "password123")
	if err != nil {
		t.Fatal(err)
	}

	for _, days := range TokenLifetimes {
		if _, _, err := s.CreateToken(ctx, u.ID, "ok", days); err != nil {
			t.Errorf("lifetime %d was rejected: %v", days, err)
		}
	}
	for _, days := range []int{0, -1, 2, 7, 366, 3650} {
		if _, _, err := s.CreateToken(ctx, u.ID, "no", days); err == nil {
			t.Errorf("lifetime %d was accepted", days)
		}
	}
}

// Tokens issued before lifetimes existed carry no deadline and keep working.
// Retrofitting one would break credentials already in use at a moment nobody
// chose.
func TestTokensWithoutADeadlineNeverLapse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := int64(1_000_000)
	s.SetNow(func() int64 { return now })

	u, err := s.CreateUser(ctx, "c@example.com", "C", RoleMember, "password123")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := s.CreateToken(ctx, u.ID, "legacy", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The shape a pre-migration row has.
	if _, err := s.pool.Exec(ctx, `UPDATE api_token SET expires_at=0 WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}

	now += 365 * 10 * 86400
	if _, err := s.UserByToken(ctx, secret); err != nil {
		t.Errorf("a token with no deadline lapsed: %v", err)
	}
	list, _ := s.ListTokens(ctx, u.ID)
	if len(list) != 1 || !list[0].Active(now) || list[0].Expired(now) {
		t.Errorf("a token with no deadline reports inactive: %+v", list)
	}
}
