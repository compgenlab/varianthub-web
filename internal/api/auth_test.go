package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/identity"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// The authorization rules are the point of this file, and they are enforced
// against real rows — a stubbed store would test the stub.
// allMigrations are every migration, discovered rather than listed.
//
// The list used to be written out by hand — and had drifted out of numeric
// order, and gone stale twice. A missing entry surfaces as `column "x" does not
// exist` in whichever test happens to touch it, rather than as anything about
// the list. Globbing means a new migration is exercised by the existing tests
// the moment it lands.
//
// Sorted, because migrations are ordered by their numeric prefix and a later one
// alters what an earlier one created.
func allMigrations(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found; the glob or the layout has moved")
	}
	sort.Strings(files)
	return files
}

type harness struct {
	server *Server
	http   http.Handler
	ids    *identity.Store
	cat    *catalog.Store
	dsn    string // schema-scoped, for tests that need a real queue
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("VHW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VHW_TEST_DATABASE_URL not set; skipping auth integration tests")
	}
	ctx := context.Background()

	var ddl strings.Builder
	for _, f := range allMigrations(t) {
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
	return &harness{server: srv, http: srv.Routes(), ids: ids, cat: cat,
		dsn: dsn + "&search_path=" + schema}
}

// withQueue gives the harness a real queue. Most tests do not need one — the
// handler rejects them before it is reached — but job ownership is decided
// after a job exists, so those tests need somewhere to put one.
func (h *harness) withQueue(t *testing.T) {
	t.Helper()
	q, err := queue.Open(context.Background(), h.dsn)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	t.Cleanup(q.Close)
	h.server.queue = q
	h.http = h.server.Routes()
}

// session creates a fresh admin account and returns a browser session for it.
//
// The web-app surface takes the credential a browser actually sends, so a test
// of those routes has to use one — a token now gets 404 there by design.
func (h *harness) session(t *testing.T) string {
	t.Helper()
	u, err := h.ids.CreateUser(context.Background(), "web@example.com", "Web",
		identity.RoleAdmin, "password123")
	if err != nil {
		t.Fatal(err)
	}
	return h.sessionFor(t, u.ID)
}

// anon issues a server-side anonymous session, the credential a visitor gets
// from loading the site.
func (h *harness) anon(t *testing.T) string {
	t.Helper()
	id, err := h.ids.CreateAnonSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// doAnon issues a request as an anonymous visitor carrying that session.
func (h *harness) doAnon(method, path, anon string, body any) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if anon != "" {
		r.AddCookie(&http.Cookie{Name: AnonCookie, Value: anon})
	}
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, r)
	return w
}

// sessionFor returns a browser session for an account that already exists, so a
// test can hold both a token and a session for the same person.
func (h *harness) sessionFor(t *testing.T, userID string) string {
	t.Helper()
	id, _, err := h.ids.CreateSession(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *harness) doSession(method, path, session string, body any) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, r)
	return w
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
	_, secret, err := h.ids.CreateToken(context.Background(), u.ID, "test", 1)
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
	_, secret, err := h.ids.CreateToken(context.Background(), u.ID, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	return u, secret
}

// The whole point of the chunk: administration takes an administrator account,
// and a token administers only because its owner does.
func TestAdminRoutesRequireAnAdminAccount(t *testing.T) {
	h := newHarness(t)
	admin, adminTok := h.admin(t)
	member, _ := h.member(t, "member@example.com")
	adminSess := h.sessionFor(t, admin.ID)
	memberSess := h.sessionFor(t, member.ID)

	for _, tc := range []struct {
		name    string
		session string
		want    int
	}{
		{"administrator", adminSess, http.StatusOK},
		{"ordinary member", memberSess, http.StatusForbidden},
		{"nobody", "", http.StatusUnauthorized},
		{"garbage", "notarealsession", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.doSession("GET", "/api/v1/admin/users", tc.session, nil)
			if got.Code != tc.want {
				t.Errorf("GET /admin/users = %d, want %d (%s)", got.Code, tc.want, got.Body.String())
			}
		})
	}

	// Administration is web-app surface, so an API token does not reach it at
	// all — not even an administrator's. That is about how much API is
	// published, not about rights, which is why the same person's session works.
	if got := h.do("GET", "/api/v1/admin/users", adminTok, nil); got.Code != http.StatusNotFound {
		t.Errorf("an admin token reached an admin route: %d", got.Code)
	}

	// Promotion takes effect on the session already issued: the role is read
	// from the account at each request, so there is nothing to reissue.
	if err := h.ids.SetRole(context.Background(), member.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if got := h.doSession("GET", "/api/v1/admin/users", memberSess, nil); got.Code != http.StatusOK {
		t.Errorf("after promotion = %d, want 200", got.Code)
	}
	// ...and so does demotion, which is what makes "only admins administer" true
	// continuously rather than only at sign-in.
	if err := h.ids.SetRole(context.Background(), member.ID, identity.RoleMember); err != nil {
		t.Fatal(err)
	}
	if got := h.doSession("GET", "/api/v1/admin/users", memberSess, nil); got.Code != http.StatusForbidden {
		t.Errorf("after demotion = %d, want 403", got.Code)
	}
}

// A revoked token stops working immediately, without touching the account.
func TestRevokedTokenLosesAccess(t *testing.T) {
	h := newHarness(t)
	u, _ := h.admin(t)
	tok, secret, err := h.ids.CreateToken(context.Background(), u.ID, "laptop", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Probed on a published route: this is about the credential, and admin
	// routes are no longer reachable with one whatever its state.
	if got := h.do("GET", "/api/v1/sources", secret, nil); got.Code != http.StatusOK {
		t.Fatalf("fresh token = %d, want 200", got.Code)
	}
	if err := h.ids.RevokeToken(context.Background(), u.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.do("GET", "/api/v1/sources", secret, nil); got.Code != http.StatusUnauthorized {
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
	admin, adminTok := h.admin(t)
	member, memberTok := h.member(t, "member@example.com")
	// Granting is administration, which the web app does with a session. The
	// token stays for the source listing below, which is published API.
	adminSess := h.sessionFor(t, admin.ID)

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
	if w := h.doSession("POST", "/api/v1/admin/sources/secret/grants", adminSess,
		map[string]string{"team_id": team.ID}); w.Code != http.StatusNoContent {
		t.Fatalf("grant = %d (%s)", w.Code, w.Body.String())
	}
	if got := names(memberTok); !got["secret"] {
		t.Error("a granted source is still hidden from the team")
	}

	// Revoking takes it away again.
	if w := h.doSession("DELETE", "/api/v1/admin/sources/secret/grants/"+team.ID, adminSess, nil); w.Code != http.StatusNoContent {
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
	// Account management is web-app surface — a session, not a token, which is
	// what the browser sends anyway.
	sess := h.sessionFor(t, u.ID)

	// /auth/me tells the UI whether to render the form at all.
	var me struct {
		CanChangePassword bool `json:"can_change_password"`
		User              struct {
			SSO bool `json:"sso"`
		} `json:"user"`
	}
	w := h.doSession("GET", "/api/v1/auth/me", sess, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if !me.CanChangePassword || me.User.SSO {
		t.Errorf("a local account reports can_change_password=%v sso=%v",
			me.CanChangePassword, me.User.SSO)
	}

	if w := h.doSession("POST", "/api/v1/auth/password", sess, map[string]string{
		"current_password": "wrong", "new_password": "new-password",
	}); w.Code != http.StatusBadRequest {
		t.Errorf("wrong current password = %d, want 400", w.Code)
	}
	if w := h.doSession("POST", "/api/v1/auth/password", sess, map[string]string{
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
	ssoSess := h.sessionFor(t, sso.ID)
	w = h.doSession("GET", "/api/v1/auth/me", ssoSess, nil)
	me.CanChangePassword = true
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.CanChangePassword || !me.User.SSO {
		t.Errorf("an SSO account reports can_change_password=%v sso=%v",
			me.CanChangePassword, me.User.SSO)
	}
	if w := h.doSession("POST", "/api/v1/auth/password", ssoSess, map[string]string{
		"current_password": "x", "new_password": "new-password",
	}); w.Code != http.StatusConflict {
		t.Errorf("SSO change = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

// With anonymous access on, an unidentified caller reaches the annotation flow
// but not administration — and /auth/me says so, which is what the UI decides
// whether to show a login wall from.
func TestAllowAnonymous(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.AllowAnonymous = true
	h.http = h.server.Routes()

	var me struct {
		Anonymous      bool `json:"anonymous"`
		Admin          bool `json:"admin"`
		AllowAnonymous bool `json:"allow_anonymous"`
	}
	visitor := h.anon(t)

	w := h.doAnon("GET", "/api/v1/auth/me", visitor, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if !me.Anonymous || me.Admin || !me.AllowAnonymous {
		t.Errorf("me = %+v; want anonymous, not admin, allow_anonymous", me)
	}

	for _, p := range []string{"/api/v1/ping", "/api/v1/snapshots", "/api/v1/sources"} {
		if got := h.doAnon("GET", p, visitor, nil); got.Code != http.StatusOK {
			t.Errorf("anonymous GET %s = %d, want 200", p, got.Code)
		}
		// Anonymous means a visitor who loaded the site, not a request with no
		// credential. Allowing the second would publish the whole API to
		// anybody, which is what a token is for.
		if got := h.do("GET", p, "", nil); got.Code != http.StatusUnauthorized {
			t.Errorf("no credential GET %s = %d, want 401", p, got.Code)
		}
		// And a session the client invented is not one we issued.
		if got := h.doAnon("GET", p, "made-up", nil); got.Code != http.StatusUnauthorized {
			t.Errorf("self-asserted session GET %s = %d, want 401", p, got.Code)
		}
	}
	// Opening the annotation flow must not open the catalog.
	if got := h.do("GET", "/api/v1/admin/users", "", nil); got.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /admin/users = %d, want 401", got.Code)
	}

	// And a private source stays invisible: anonymous is in no team, so it is
	// granted nothing.
	if err := h.cat.PutSource(context.Background(), catalog.Source{
		ID: "secret", Name: "secret", Version: "1", Kind: "vcf", Build: "GRCh38",
		Visibility: catalog.VisibilityPrivate,
		TOML:       "[[sources]]\nname = \"secret\"\nversion = \"1\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	w = h.doAnon("GET", "/api/v1/sources", visitor, nil)
	if strings.Contains(w.Body.String(), "secret") {
		t.Errorf("a private source is visible anonymously: %s", w.Body.String())
	}
}

// An anonymous submitter must be able to read back their own job, and only
// their own. Without a scope there is nothing to match on, and the alternative
// to a 404 would be showing them everyone's.
func TestAnonymousJobHistoryIsScoped(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.AllowAnonymous = true
	h.withQueue(t)

	submit := func(session string) string {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/v1/annotate",
			strings.NewReader(`{"snapshot":"s","variants":["chr1:100:A:G"]}`))
		r.Header.Set("Content-Type", "application/json")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: AnonCookie, Value: session})
		}
		w := httptest.NewRecorder()
		h.http.ServeHTTP(w, r)
		if w.Code != http.StatusAccepted && w.Code != http.StatusOK {
			t.Fatalf("submit = %d (%s)", w.Code, w.Body.String())
		}
		var body struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.JobID
	}
	read := func(id, session string) int {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/v1/jobs/"+id, nil)
		if session != "" {
			r.AddCookie(&http.Cookie{Name: AnonCookie, Value: session})
		}
		w := httptest.NewRecorder()
		h.http.ServeHTTP(w, r)
		return w.Code
	}

	browserA, browserB := h.anon(t), h.anon(t)

	mine := submit(browserA)
	if got := read(mine, browserA); got != http.StatusOK {
		t.Errorf("reading my own job = %d, want 200", got)
	}
	// Another browser CAN read it, holding the id. An anonymous job has no
	// account to be private to, so its link is the credential and sharing the
	// link is how a result gets shared. What stays scoped is the history
	// listing, asserted below: holding one link does not enumerate the rest.
	if got := read(mine, browserB); got != http.StatusOK {
		t.Errorf("another browser reading a shared anonymous link = %d, want 200", got)
	}
	// And with no credential at all — curl with a URL and nothing else. This
	// used to be a 401, refused before anyone asked whose job it was, which
	// made a "shareable" link unusable by anything but a browser that had been
	// handed a session on page load.
	if got := read(mine, ""); got != http.StatusOK {
		t.Errorf("a bare read with no credential = %d, want 200", got)
	}

	// The listing stays scoped, and this is what carries the privacy now that a
	// link is readable: browserB can open a link it was given, and still cannot
	// discover what else browserA has run. Sharing one result shares one result.
	list := func(session string) string {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/v1/jobs", nil)
		r.AddCookie(&http.Cookie{Name: AnonCookie, Value: session})
		w := httptest.NewRecorder()
		h.http.ServeHTTP(w, r)
		return w.Body.String()
	}
	if strings.Contains(list(browserB), mine) {
		t.Error("another browser's job history included a job it did not submit")
	}
	if !strings.Contains(list(browserA), mine) {
		t.Error("the submitting browser's own history did not include its job")
	}
}

// The register form asks where data should go before writing the manifest, so
// validation has to say whether there will be any. Without this the form cannot
// tell a source that is ready to use from one that still needs fetching.
func TestValidateReportsWhetherDataIsNeeded(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.admin(t)
	adminSess := h.sessionFor(t, admin.ID)

	for _, tc := range []struct {
		name      string
		toml      string
		needsData bool
		stream    bool
	}{
		{
			name: "a plain data source needs a download",
			toml: "[[sources]]\nname=\"revel\"\nversion=\"1.3\"\nformat=\"tab\"\n" +
				"assembly=\"GRCh38\"\nurl=\"https://example.org/revel.zip\"\n",
			needsData: true,
		},
		{
			name: "a streamed source is read from its origin",
			toml: "[[sources]]\nname=\"gnomad\"\nversion=\"4.1\"\nassembly=\"GRCh38\"\n" +
				"stream=true\nurl=\"https://example.org/g.vcf.bgz\"\n",
			needsData: false, stream: true,
		},
		{
			name:      "a builtin computes from the variant",
			toml:      "[[sources]]\nname=\"builtins\"\nversion=\"1\"\ntype=\"builtin\"\n",
			needsData: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.doSession("POST", "/api/v1/admin/sources/validate", adminSess,
				map[string]string{"toml": tc.toml})
			if w.Code != http.StatusOK {
				t.Fatalf("validate = %d (%s)", w.Code, w.Body.String())
			}
			var got struct {
				Valid     bool `json:"valid"`
				NeedsData bool `json:"needs_data"`
				Stream    bool `json:"stream"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !got.Valid {
				t.Fatalf("manifest rejected: %s", w.Body.String())
			}
			if got.NeedsData != tc.needsData || got.Stream != tc.stream {
				t.Errorf("needs_data=%v stream=%v; want %v/%v",
					got.NeedsData, got.Stream, tc.needsData, tc.stream)
			}
		})
	}
}

// Re-registering a manifest must not delete the helper files it names.
//
// PutAssets replaces the stored set, so a POST that changed one line of TOML and
// said nothing about assets wiped them — and the failure appeared later, at the
// next annotation, as a tool unable to open a script, with nothing connecting it
// to the edit. This destroyed VEP's two scripts in the dev stack.
func TestReRegisteringKeepsAssets(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.admin(t)
	sess := h.sessionFor(t, admin.ID)

	manifest := "[[sources]]\ntype=\"tool\"\nname=\"vep\"\nversion=\"113\"\nassembly=\"GRCh38\"\n" +
		"  [[sources.steps]]\n  run=\"python3 {workdir}/helper.py\"\n"
	body := map[string]any{
		"toml": manifest,
		"assets": []map[string]string{
			{"name": "helper.py", "content": "print('hi')\n"},
		},
	}
	if rec := h.doSession("POST", "/api/v1/admin/sources", sess, body); rec.Code != 200 {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}

	// The same manifest with one line changed, and nothing said about assets.
	body2 := map[string]any{"toml": manifest + "  # a comment\n"}
	rec := h.doSession("POST", "/api/v1/admin/sources", sess, body2)
	if rec.Code != 200 {
		t.Fatalf("re-register = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"assets":1`) {
		t.Errorf("re-registering reported %s; the stored asset should survive",
			rec.Body.String())
	}

	// An explicit empty list still clears, so "remove them" remains expressible.
	body3 := map[string]any{"toml": manifest, "assets": []map[string]string{}}
	if rec = h.doSession("POST", "/api/v1/admin/sources", sess, body3); rec.Code != 200 {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"assets":0`) {
		t.Errorf("an explicit empty list did not clear the assets: %s", rec.Body.String())
	}
}
