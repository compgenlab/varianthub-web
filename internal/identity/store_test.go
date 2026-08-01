package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A real Postgres, one schema per test — same shape as the catalog tests. The
// auth tables reference `source`, so the catalog migrations come along too.
var migrationFiles = []string{
	"../../migrations/0001_job_queue.sql",
	"../../migrations/0002_catalog.sql",
	"../../migrations/0004_registry.sql",
	"../../migrations/0005_adhoc_snapshot.sql",
	"../../migrations/0006_storage.sql",
	"../../migrations/0007_auth.sql",
	"../../migrations/0008_default_private.sql",
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping identity store tests")
	}
	ctx := context.Background()

	var ddl strings.Builder
	for _, f := range migrationFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ddl.Write(b)
		ddl.WriteString("\n")
	}

	schema := fmt.Sprintf("i_%d", time.Now().UnixNano())
	setup, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := setup.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		setup.Close()
		t.Fatalf("create schema: %v", err)
	}
	if _, err := setup.Exec(ctx, `SET search_path TO `+schema+`; `+ddl.String()); err != nil {
		setup.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	setup.Close()

	pool, err := pgxpool.New(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if drop, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			drop.Close()
		}
		pool.Close()
	})
	return NewStore(pool)
}

func mustUser(t *testing.T, s *Store, email string) User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), email, "Test User", RoleMember, "pw-"+email)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	return u
}

func TestUserLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	u := mustUser(t, s, "Ada@Example.COM")
	if u.Email != "ada@example.com" {
		t.Errorf("email not normalized: %q", u.Email)
	}

	// The same address in different case is the same person, not a second one.
	if _, err := s.CreateUser(ctx, "ADA@example.com", "", RoleMember, "password123"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate email by case: %v; want ErrExists", err)
	}

	got, err := s.Authenticate(ctx, "ada@EXAMPLE.com", "pw-Ada@Example.COM")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("authenticated as %q, want %q", got.ID, u.ID)
	}

	// A wrong password and an unknown account must be indistinguishable, or the
	// error itself tells an attacker which addresses have accounts.
	_, wrongPw := s.Authenticate(ctx, "ada@example.com", "nope")
	_, noSuch := s.Authenticate(ctx, "nobody@example.com", "nope")
	if wrongPw == nil || noSuch == nil {
		t.Fatal("bad credentials accepted")
	}
	if wrongPw.Error() != noSuch.Error() {
		t.Errorf("errors distinguish the two cases:\n  wrong password: %v\n  no such user:   %v", wrongPw, noSuch)
	}

	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, u.Email, "pw-Ada@Example.COM"); err == nil {
		t.Error("a disabled account still authenticates")
	}
}

// Locking out every administrator would leave a catalog nobody can manage and
// no supported way back in, so the store refuses the last one.
func TestLastAdminIsProtected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	admin, err := s.CreateUser(ctx, "root@example.com", "Root", RoleAdmin, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, admin.ID, RoleMember); err == nil {
		t.Error("demoting the only administrator succeeded")
	}
	if err := s.SetDisabled(ctx, admin.ID, true); err == nil {
		t.Error("disabling the only administrator succeeded")
	}

	second, err := s.CreateUser(ctx, "root2@example.com", "Root2", RoleAdmin, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, admin.ID, RoleMember); err != nil {
		t.Errorf("demoting one of two administrators: %v", err)
	}
	// ...and now the second is the last one.
	if err := s.SetDisabled(ctx, second.ID, true); err == nil {
		t.Error("disabling the now-only administrator succeeded")
	}
}

// A user holds several tokens at once — one per machine or script — so losing
// one means revoking that one, not re-issuing all of them.
func TestTokensAreIndependent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "dev@example.com")

	laptop, laptopSecret, err := s.CreateToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	cluster, clusterSecret, err := s.CreateToken(ctx, u.ID, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	if laptopSecret == clusterSecret {
		t.Fatal("two tokens share a secret")
	}
	if !strings.HasPrefix(laptopSecret, TokenPrefix) {
		t.Errorf("secret lacks the scannable prefix: %q", laptopSecret)
	}

	for _, secret := range []string{laptopSecret, clusterSecret} {
		got, err := s.UserByToken(ctx, secret)
		if err != nil {
			t.Fatalf("UserByToken: %v", err)
		}
		if got.ID != u.ID {
			t.Errorf("token resolved to %q, want %q", got.ID, u.ID)
		}
	}

	// Revoking one must leave the other working.
	if err := s.RevokeToken(ctx, u.ID, laptop.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByToken(ctx, laptopSecret); err == nil {
		t.Error("a revoked token still authenticates")
	}
	if _, err := s.UserByToken(ctx, clusterSecret); err != nil {
		t.Errorf("revoking one token broke another: %v", err)
	}

	// One user may not revoke another's token.
	other := mustUser(t, s, "other@example.com")
	if err := s.RevokeToken(ctx, other.ID, cluster.ID); err == nil {
		t.Error("revoked another user's token")
	}

	list, err := s.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d tokens, want 2 (revoked ones stay visible)", len(list))
	}
	for _, tk := range list {
		if strings.Contains(clusterSecret, tk.Prefix) && !tk.Active() {
			t.Error("the live token reports revoked")
		}
		if tk.ID == laptop.ID && tk.Active() {
			t.Error("the revoked token reports active")
		}
		if tk.ID == cluster.ID && tk.LastUsedAt == 0 {
			t.Error("last_used_at not recorded after a successful authentication")
		}
	}
	_ = cluster
}

// Last-used is what makes an unused token safe to delete, so it has to move.
func TestTokenLastUsedAdvances(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "clock@example.com")

	now := int64(1000)
	s.SetNow(func() int64 { return now })

	tk, secret, err := s.CreateToken(ctx, u.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	if tk.LastUsedAt != 0 {
		t.Errorf("a fresh token reports last-used %d", tk.LastUsedAt)
	}

	now = 2000
	if _, err := s.UserByToken(ctx, secret); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].LastUsedAt != 2000 {
		t.Errorf("last_used_at = %d, want 2000", list[0].LastUsedAt)
	}

	now = 3000
	if _, err := s.UserByToken(ctx, secret); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListTokens(ctx, u.ID)
	if list[0].LastUsedAt != 3000 {
		t.Errorf("last_used_at did not advance on reuse: %d", list[0].LastUsedAt)
	}
}

// The prefix identifies the row; it must never be enough to authenticate.
func TestPrefixDoesNotAuthenticate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "prefix@example.com")

	tk, secret, err := s.CreateToken(ctx, u.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByToken(ctx, tk.Prefix); err == nil {
		t.Error("the prefix alone authenticated")
	}
	// Right prefix, wrong secret — the row is found, the hash must reject it.
	if _, err := s.UserByToken(ctx, tk.Prefix+strings.Repeat("a", 32)); err == nil {
		t.Error("a forged secret with a valid prefix authenticated")
	}
	if _, err := s.UserByToken(ctx, secret); err != nil {
		t.Errorf("the real secret was rejected: %v", err)
	}
}

func TestSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "sess@example.com")

	now := int64(1000)
	s.SetNow(func() int64 { return now })

	id, exp, err := s.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UserBySession(ctx, id)
	if err != nil {
		t.Fatalf("UserBySession: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("session resolved to %q", got.ID)
	}

	now = exp + 1
	if _, err := s.UserBySession(ctx, id); err == nil {
		t.Error("an expired session still resolves")
	}
	n, err := s.PurgeExpiredSessions(ctx)
	if err != nil || n != 1 {
		t.Errorf("PurgeExpiredSessions = %d, %v; want 1, nil", n, err)
	}

	now = 5000
	id, _, _ = s.CreateSession(ctx, u.ID)
	if err := s.EndSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySession(ctx, id); err == nil {
		t.Error("a logged-out session still resolves")
	}
}

func TestTeamsAndGrants(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	alice := mustUser(t, s, "alice@example.com")
	bob := mustUser(t, s, "bob@example.com")

	lab, err := s.CreateTeam(ctx, "Lab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "lab"); !errors.Is(err, ErrExists) {
		t.Errorf("a team name differing only in case was accepted: %v", err)
	}
	if err := s.AddMember(ctx, lab.ID, alice.ID, TeamOwner); err != nil {
		t.Fatal(err)
	}
	members, err := s.Members(ctx, lab.ID)
	if err != nil || len(members) != 1 || members[0].Role != TeamOwner {
		t.Fatalf("Members = %+v, %v", members, err)
	}

	// Grants attach to teams, so a private source needs a team in common.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO source (id,name,version,kind,build,visibility,toml_text,created_at,updated_at)
		 VALUES ('priv','priv','1','vcf','GRCh38','private','',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(ctx, "priv", lab.ID, alice.ID); err != nil {
		t.Fatal(err)
	}

	aliceTeams, _ := s.TeamIDsFor(ctx, alice.ID)
	granted, err := s.GrantedSourceIDs(ctx, aliceTeams)
	if err != nil {
		t.Fatal(err)
	}
	if !granted["priv"] {
		t.Error("a team member cannot see the source granted to their team")
	}

	bobTeams, _ := s.TeamIDsFor(ctx, bob.ID)
	granted, _ = s.GrantedSourceIDs(ctx, bobTeams)
	if granted["priv"] {
		t.Error("a non-member can see a private source")
	}

	// Removing the member removes the access, without touching the grant.
	if err := s.RemoveMember(ctx, lab.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	aliceTeams, _ = s.TeamIDsFor(ctx, alice.ID)
	granted, _ = s.GrantedSourceIDs(ctx, aliceTeams)
	if granted["priv"] {
		t.Error("access survived removal from the team")
	}

	// Deleting the team takes the grant with it, so the source does not stay
	// reachable through a team that no longer exists.
	if err := s.AddMember(ctx, lab.ID, alice.ID, TeamMember); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeam(ctx, lab.ID); err != nil {
		t.Fatal(err)
	}
	teams, _ := s.GrantsFor(ctx, "priv")
	if len(teams) != 0 {
		t.Errorf("grants outlived the team: %+v", teams)
	}
}

// Resolve is what every request runs; an unusable credential must land on
// anonymous rather than an error page.
func TestResolve(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "resolve@example.com")
	lab, _ := s.CreateTeam(ctx, "Resolvers")
	if err := s.AddMember(ctx, lab.ID, u.ID, TeamMember); err != nil {
		t.Fatal(err)
	}
	_, secret, _ := s.CreateToken(ctx, u.ID, "t")
	sess, _, _ := s.CreateSession(ctx, u.ID)

	for _, tc := range []struct {
		name            string
		bearer, session string
		wantUser        bool
	}{
		{"token", secret, "", true},
		{"session", "", sess, true},
		{"both", secret, sess, true},
		{"nothing", "", "", false},
		{"garbage token", TokenPrefix + "deadbeef", "", false},
		{"garbage session", "", "not-a-session", false},
		{"non-token bearer", "Basic abc", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := s.Resolve(ctx, tc.bearer, tc.session)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := !c.Anonymous(); got != tc.wantUser {
				t.Fatalf("identified = %v, want %v", got, tc.wantUser)
			}
			if tc.wantUser && !c.InTeam(lab.ID) {
				t.Error("team membership not carried on the caller")
			}
		})
	}
}
