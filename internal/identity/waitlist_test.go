package identity

import (
	"context"
	"errors"
	"testing"
)

func TestAccessRequestIsOnePerIdentityHoweverOftenTheyTry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, err := s.RecordAccessRequest(ctx, "cilogon", "sub-1", "Ann@Example.org", "Ann")
	if err != nil {
		t.Fatalf("RecordAccessRequest: %v", err)
	}
	if first.Email != "ann@example.org" {
		t.Errorf("email = %q, want it normalized", first.Email)
	}

	// Trying again is the same request, seen again — not a second place in the
	// queue, and not a way to jump it.
	s.nowFn = func() int64 { return first.CreatedAt + 3600 }
	again, err := s.RecordAccessRequest(ctx, "cilogon", "sub-1", "ann@example.org", "Ann N")
	if err != nil {
		t.Fatalf("second RecordAccessRequest: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("a second attempt made a new request (%s then %s)", first.ID, again.ID)
	}
	if again.CreatedAt != first.CreatedAt {
		t.Errorf("created_at moved: %d then %d — the queue order would change",
			first.CreatedAt, again.CreatedAt)
	}
	if again.LastSeenAt <= first.LastSeenAt {
		t.Error("last_seen_at did not move; still-trying looks like gave-up")
	}
	// The provider is authoritative for the name, so a change comes through.
	if again.Name != "Ann N" {
		t.Errorf("name = %q, want it refreshed from the provider", again.Name)
	}

	all, err := s.ListAccessRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d requests, want 1", len(all))
	}
}

// The point of the whole feature: approving makes the next sign-in ordinary.
func TestApprovingCreatesTheAccountAndLinksTheIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	req, err := s.RecordAccessRequest(ctx, "cilogon", "sub-2", "bob@example.org", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.ApproveAccessRequest(ctx, req.ID, "", RoleMember)
	if err != nil {
		t.Fatalf("ApproveAccessRequest: %v", err)
	}
	if u.Email != "bob@example.org" || u.Role != RoleMember {
		t.Errorf("account = %+v, want bob@example.org as a member", u)
	}
	if u.Tier != "standard" {
		t.Errorf("tier = %q, want standard", u.Tier)
	}

	// Linked by subject, not left to the address to match on a later sign-in —
	// an institution reassigning an address must not turn them into a stranger.
	got, err := s.UserByIdentity(ctx, "cilogon", "sub-2")
	if err != nil {
		t.Fatalf("UserByIdentity after approval: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("identity resolves to %s, want the approved account %s", got.ID, u.ID)
	}

	// Approving twice is not an error: the intent is already satisfied, and a
	// double-click should not produce one.
	again, err := s.ApproveAccessRequest(ctx, req.ID, "", RoleMember)
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if again.ID != u.ID {
		t.Errorf("second approve made a different account: %s then %s", u.ID, again.ID)
	}
}

// Declining has to stick. If the next sign-in reopened the request, an
// administrator would decline the same person forever.
func TestDecliningSurvivesTheNextAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	req, err := s.RecordAccessRequest(ctx, "cilogon", "sub-3", "spam@example.org", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeclineAccessRequest(ctx, req.ID, ""); err != nil {
		t.Fatalf("DeclineAccessRequest: %v", err)
	}

	if _, err := s.RecordAccessRequest(ctx, "cilogon", "sub-3", "spam@example.org", ""); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	all, err := s.ListAccessRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d requests, want 1", len(all))
	}
	if all[0].Status != RequestDeclined {
		t.Errorf("status = %q after trying again, want it to stay declined", all[0].Status)
	}
}

func TestDecidingSomethingThatIsNotThere(t *testing.T) {
	s := testStore(t)
	if err := s.DeclineAccessRequest(context.Background(), "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.ApproveAccessRequest(context.Background(), "nope", "", RoleMember); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// An address that already has an account is not a reason to fail: somebody an
// administrator added by hand between the request and the decision should end
// up linked to that account, not blocked by it.
func TestApprovingAnAddressThatAlreadyHasAnAccount(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	existing, err := s.CreateUser(ctx, "carol@example.org", "Carol", RoleMember, "")
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.RecordAccessRequest(ctx, "cilogon", "sub-4", "carol@example.org", "Carol")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.ApproveAccessRequest(ctx, req.ID, "", RoleMember)
	if err != nil {
		t.Fatalf("ApproveAccessRequest: %v", err)
	}
	if u.ID != existing.ID {
		t.Errorf("approval made a second account for one address: %s then %s", existing.ID, u.ID)
	}
	got, err := s.UserByIdentity(ctx, "cilogon", "sub-4")
	if err != nil || got.ID != existing.ID {
		t.Errorf("identity did not link to the existing account: %+v %v", got, err)
	}
}
