package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/identity"
)

// The authorization rules are the point of this file, and they are enforced
// against real rows — a stubbed store would test the stub.
var authMigrations = []string{
	"../../migrations/0001_job_queue.sql",
	"../../migrations/0002_catalog.sql",
	"../../migrations/0003_job_variant.sql",
	"../../migrations/0004_registry.sql",
	"../../migrations/0005_adhoc_snapshot.sql",
	"../../migrations/0006_storage.sql",
	"../../migrations/0007_auth.sql",
	"../../migrations/0008_default_private.sql",
	"../../migrations/0009_bootstrap.sql",
	"../../migrations/0010_job_user.sql",
}

type harness struct {
	server *Server
	http   http.Handler
	ids    *identity.Store
	cat    *catalog.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping auth integration tests")
	}
	ctx := context.Background()

	var ddl strings.Builder
	for _, f := range authMigrations {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ddl.Write(b)
		ddl.WriteString("\n")
	}
	schema := fmt.Sprintf("a_%d", time.Now().UnixNano())
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
		t.Fatalf("migrate: %v", err)
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

	cat := catalog.New(pool)
	ids := identity.NewStore(pool)
	srv := New(&config.Config{
		Version: "test", RatePerMin: 1000, RateBurst: 1000,
	}, nil, cat, ids, nil)
	return &harness{server: srv, http: srv.Routes(), ids: ids, cat: cat}
}

// do issues a request, optionally as a bearer credential.
func (h *harness) do(method, path, bearer string, body any) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, r)
	return w
}

func (h *harness) admin(t *testing.T) (identity.User, string) {
	t.Helper()
	u, err := h.ids.CreateUser(context.Background(), "admin@example.com", "Admin",
		identity.RoleAdmin, "password123")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := h.ids.CreateToken(context.Background(), u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return u, secret
}

func (h *harness) member(t *testing.T, email string) (identity.User, string) {
	t.Helper()
	u, err := h.ids.CreateUser(context.Background(), email, "Member",
		identity.RoleMember, "password123")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := h.ids.CreateToken(context.Background(), u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return u, secret
}

// The whole point of the chunk: administration takes an administrator account,
// and a token administers only because its owner does.
func TestAdminRoutesRequireAnAdminAccount(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.admin(t)
	member, memberTok := h.member(t, "member@example.com")

	for _, tc := range []struct {
		name   string
		bearer string
		want   int
	}{
		{"administrator", adminTok, http.StatusOK},
		{"ordinary member", memberTok, http.StatusForbidden},
		{"nobody", "", http.StatusUnauthorized},
		{"garbage", identity.TokenPrefix + "notreal", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.do("GET", "/api/v1/admin/users", tc.bearer, nil); got.Code != tc.want {
				t.Errorf("GET /admin/users = %d, want %d (%s)", got.Code, tc.want, got.Body.String())
			}
		})
	}

	// Promotion takes effect on the token already issued: the role is read from
	// the account at each request, so there is nothing to reissue.
	if err := h.ids.SetRole(context.Background(), member.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if got := h.do("GET", "/api/v1/admin/users", memberTok, nil); got.Code != http.StatusOK {
		t.Errorf("after promotion = %d, want 200", got.Code)
	}
	// ...and so does demotion, which is what makes "only admins hold admin
	// tokens" true continuously rather than only at issue time.
	if err := h.ids.SetRole(context.Background(), member.ID, identity.RoleMember); err != nil {
		t.Fatal(err)
	}
	if got := h.do("GET", "/api/v1/admin/users", memberTok, nil); got.Code != http.StatusForbidden {
		t.Errorf("after demotion = %d, want 403", got.Code)
	}
}

// A revoked token stops working immediately, without touching the account.
func TestRevokedTokenLosesAccess(t *testing.T) {
	h := newHarness(t)
	u, _ := h.admin(t)
	tok, secret, err := h.ids.CreateToken(context.Background(), u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.do("GET", "/api/v1/admin/users", secret, nil); got.Code != http.StatusOK {
		t.Fatalf("fresh token = %d, want 200", got.Code)
	}
	if err := h.ids.RevokeToken(context.Background(), u.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.do("GET", "/api/v1/admin/users", secret, nil); got.Code != http.StatusUnauthorized {
		t.Errorf("revoked token = %d, want 401", got.Code)
	}
}

// The bootstrap credential creates the first administrator and then stops
// working — otherwise a startup log would remain a way in forever.
func TestBootstrapIsSpentOnFirstAdmin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	needs, err := h.ids.NeedsBootstrap(ctx)
	if err != nil || !needs {
		t.Fatalf("a fresh installation reports needs_bootstrap=%v (%v)", needs, err)
	}
	secret, err := h.ids.IssueBootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, identity.BootstrapPrefix) {
		t.Errorf("bootstrap secret is not marked as one: %q", secret)
	}

	// It administers, which is what makes creating the first account possible.
	w := h.do("POST", "/api/v1/admin/users", secret, map[string]string{
		"email": "root@example.com", "password": "password123", "role": "admin",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("bootstrap create = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	// And now it does not.
	if got := h.do("GET", "/api/v1/admin/users", secret, nil); got.Code != http.StatusUnauthorized {
		t.Errorf("spent bootstrap = %d, want 401", got.Code)
	}
	if needs, _ := h.ids.NeedsBootstrap(ctx); needs {
		t.Error("still reports needing a bootstrap after an administrator exists")
	}

	// The account it created is a real one that signs in with a password.
	w = h.do("POST", "/api/v1/auth/login", "", map[string]string{
		"email": "root@example.com", "password": "password123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			cookie = c.Value
			if !c.HttpOnly {
				t.Error("the session cookie is readable by scripts")
			}
		}
	}
	if cookie == "" {
		t.Fatal("login set no session cookie")
	}

	r := httptest.NewRequest("GET", "/api/v1/admin/users", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("session-authenticated admin request = %d, want 200", rec.Code)
	}
}

// A bootstrap token must not be a permanent back door if it is never spent: an
// administrator existing at all is enough to close it.
func TestBootstrapDiesWhenAnAdminExists(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	secret, err := h.ids.IssueBootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ids.CreateUser(ctx, "someone@example.com", "A",
		identity.RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if got := h.do("GET", "/api/v1/admin/users", secret, nil); got.Code != http.StatusUnauthorized {
		t.Errorf("unspent bootstrap after an admin exists = %d, want 401", got.Code)
	}
}

// Private is the default, and a private source is invisible without a grant.
func TestPrivateSourcesNeedAGrant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, adminTok := h.admin(t)
	member, memberTok := h.member(t, "member@example.com")

	put := func(id, vis string) {
		t.Helper()
		if err := h.cat.PutSource(ctx, catalog.Source{
			ID: id, Name: id, Version: "1", Kind: "vcf", Build: "GRCh38",
			Visibility: vis,
			TOML:       "[[sources]]\nname = \"" + id + "\"\nversion = \"1\"\n",
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("open", catalog.VisibilityPublic)
	put("secret", catalog.VisibilityPrivate)

	names := func(tok string) map[string]bool {
		t.Helper()
		w := h.do("GET", "/api/v1/sources", tok, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /sources = %d (%s)", w.Code, w.Body.String())
		}
		var body struct {
			Sources []struct {
				ID string `json:"id"`
			} `json:"sources"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, s := range body.Sources {
			out[s.ID] = true
		}
		return out
	}

	got := names(memberTok)
	if !got["open"] {
		t.Error("a public source is not listed for a member")
	}
	if got["secret"] {
		t.Error("a private source is listed without a grant")
	}
	if got := names(adminTok); !got["secret"] {
		t.Error("an administrator cannot see a private source")
	}

	// Grant it to a team the member belongs to.
	team, err := h.ids.CreateTeam(ctx, "Lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ids.AddMember(ctx, team.ID, member.ID, identity.TeamMember); err != nil {
		t.Fatal(err)
	}
	if w := h.do("POST", "/api/v1/admin/sources/secret/grants", adminTok,
		map[string]string{"team_id": team.ID}); w.Code != http.StatusNoContent {
		t.Fatalf("grant = %d (%s)", w.Code, w.Body.String())
	}
	if got := names(memberTok); !got["secret"] {
		t.Error("a granted source is still hidden from the team")
	}

	// Revoking takes it away again.
	if w := h.do("DELETE", "/api/v1/admin/sources/secret/grants/"+team.ID, adminTok, nil); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d (%s)", w.Code, w.Body.String())
	}
	if got := names(memberTok); got["secret"] {
		t.Error("a revoked grant still shows the source")
	}
}

// A snapshot pinning a source the caller cannot see is hidden whole — and
// stays unusable even when its name is guessed, or hiding it would be cosmetic.
func TestSnapshotsWithPrivateSourcesAreHidden(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, adminTok := h.admin(t)
	_, memberTok := h.member(t, "member@example.com")

	for id, vis := range map[string]string{
		"pub": catalog.VisibilityPublic, "priv": catalog.VisibilityPrivate,
	} {
		if err := h.cat.PutSource(ctx, catalog.Source{
			ID: id, Name: id, Version: "1", Kind: "vcf", Build: "GRCh38", Visibility: vis,
			TOML: "[[sources]]\nname = \"" + id + "\"\nversion = \"1\"\n",
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(id string, sources ...string) {
		t.Helper()
		if err := h.cat.PutSnapshot(ctx, catalog.Snapshot{
			ID: id, Build: "GRCh38", State: catalog.StatePublished,
		}, sources); err != nil {
			t.Fatal(err)
		}
	}
	mk("open-snap", "pub")
	mk("mixed-snap", "pub", "priv")

	listed := func(tok string) map[string]bool {
		t.Helper()
		w := h.do("GET", "/api/v1/snapshots?state=all", tok, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /snapshots = %d (%s)", w.Code, w.Body.String())
		}
		var body struct {
			Snapshots []struct {
				ID string `json:"id"`
			} `json:"snapshots"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, s := range body.Snapshots {
			out[s.ID] = true
		}
		return out
	}

	got := listed(memberTok)
	if !got["open-snap"] {
		t.Error("a fully public snapshot is hidden")
	}
	if got["mixed-snap"] {
		t.Error("a snapshot pinning a private source is listed")
	}
	if got := listed(adminTok); !got["mixed-snap"] {
		t.Error("an administrator cannot see the mixed snapshot")
	}

	// Guessing the name gets a 404, not a 403 — the name and its existence are
	// themselves information about what this installation holds.
	if w := h.do("GET", "/api/v1/snapshots/mixed-snap", memberTok, nil); w.Code != http.StatusNotFound {
		t.Errorf("direct fetch = %d, want 404", w.Code)
	}

	// And it cannot be annotated against, which is the part that would
	// otherwise make the hiding purely cosmetic.
	w := h.do("POST", "/api/v1/annotate", memberTok, map[string]any{
		"snapshot": "mixed-snap", "variants": []string{"chr1:100:A:G"},
	})
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Errorf("annotate against a hidden snapshot = %d, want a refusal (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mixed-snap") {
		t.Errorf("refusal does not name the snapshot: %s", w.Body.String())
	}
}

// Selecting a private source by ref must be refused too, or the ad-hoc path
// would be a way around the snapshot rule.
func TestAdhocSelectionRefusesHiddenSources(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, memberTok := h.member(t, "member@example.com")

	if err := h.cat.PutSource(ctx, catalog.Source{
		ID: "priv", Name: "priv", Version: "1", Kind: "vcf", Build: "GRCh38",
		Visibility: catalog.VisibilityPrivate,
		TOML:       "[[sources]]\nname = \"priv\"\nversion = \"1\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	w := h.do("POST", "/api/v1/annotate", memberTok, map[string]any{
		"sources": []string{"priv"}, "build": "GRCh38",
		"variants": []string{"chr1:100:A:G"},
	})
	if w.Code == http.StatusOK || w.Code == http.StatusAccepted {
		t.Fatalf("annotating with an ungranted private source succeeded (%d)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "priv") {
		t.Errorf("refusal does not name the source: %s", w.Body.String())
	}
}

// Signing out invalidates the session server-side, not just in the browser.
func TestLogoutEndsTheSession(t *testing.T) {
	h := newHarness(t)
	u, _ := h.admin(t)

	id, _, err := h.ids.CreateSession(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	withCookie := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: id})
		w := httptest.NewRecorder()
		h.http.ServeHTTP(w, r)
		return w
	}
	if w := withCookie("GET", "/api/v1/admin/users"); w.Code != http.StatusOK {
		t.Fatalf("session request = %d, want 200", w.Code)
	}
	if w := withCookie("POST", "/api/v1/auth/logout"); w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", w.Code)
	}
	if w := withCookie("GET", "/api/v1/admin/users"); w.Code != http.StatusUnauthorized {
		t.Errorf("after logout = %d, want 401", w.Code)
	}
}

// The change-password endpoint over the wire, including the SSO refusal.
func TestChangePasswordEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	u, err := h.ids.CreateUser(ctx, "local@example.com", "Local", identity.RoleMember, "old-password")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := h.ids.CreateToken(ctx, u.ID, "t")
	if err != nil {
		t.Fatal(err)
	}

	// /auth/me tells the UI whether to render the form at all.
	var me struct {
		CanChangePassword bool `json:"can_change_password"`
		User              struct {
			SSO bool `json:"sso"`
		} `json:"user"`
	}
	w := h.do("GET", "/api/v1/auth/me", secret, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if !me.CanChangePassword || me.User.SSO {
		t.Errorf("a local account reports can_change_password=%v sso=%v",
			me.CanChangePassword, me.User.SSO)
	}

	if w := h.do("POST", "/api/v1/auth/password", secret, map[string]string{
		"current_password": "wrong", "new_password": "new-password",
	}); w.Code != http.StatusBadRequest {
		t.Errorf("wrong current password = %d, want 400", w.Code)
	}
	if w := h.do("POST", "/api/v1/auth/password", secret, map[string]string{
		"current_password": "old-password", "new_password": "new-password",
	}); w.Code != http.StatusNoContent {
		t.Fatalf("change = %d, want 204 (%s)", w.Code, w.Body.String())
	}
	if _, err := h.ids.Authenticate(ctx, u.Email, "new-password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}

	// An SSO account is refused by the endpoint, not only hidden in the UI.
	sso, err := h.ids.CreateUser(ctx, "sso@example.com", "Federated", identity.RoleMember, "")
	if err != nil {
		t.Fatal(err)
	}
	_, ssoTok, err := h.ids.CreateToken(ctx, sso.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	w = h.do("GET", "/api/v1/auth/me", ssoTok, nil)
	me.CanChangePassword = true
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.CanChangePassword || !me.User.SSO {
		t.Errorf("an SSO account reports can_change_password=%v sso=%v",
			me.CanChangePassword, me.User.SSO)
	}
	if w := h.do("POST", "/api/v1/auth/password", ssoTok, map[string]string{
		"current_password": "x", "new_password": "new-password",
	}); w.Code != http.StatusConflict {
		t.Errorf("SSO change = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}
