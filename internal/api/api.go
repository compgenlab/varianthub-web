// Package api is the HTTP surface.
//
// Ops endpoints (/healthz, /version) are open. Everything under /api/v1 resolves
// an identity once in withCaller; /api/v1/admin then requires an administrator
// account, and the rest requires any identity unless VHW_ALLOW_ANONYMOUS is set.
// The v1 handlers live in v1.go, identity in identity.go.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/identity"
	"github.com/compgenlab/varianthub-web/internal/limit"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Server is the API server.
type Server struct {
	cfg      *config.Config
	queue    *queue.Queue
	catalog  *catalog.Store  // nil disables the catalog endpoints
	identity *identity.Store // nil disables accounts (open/legacy deployments)
	spa      *SPA            // nil serves no web UI (API-only)
	trusted  []*net.IPNet
	limiter  *limit.Limiter
	remote   *remoteSizer
	oidc     *oidcProvider // nil when no external sign-in is configured
}

// New builds the server. cat may be nil, in which case the catalog endpoints
// report 503 rather than the process refusing to start -- annotation submission
// and job polling do not need the catalog.
func New(cfg *config.Config, q *queue.Queue, cat *catalog.Store, ids *identity.Store, spa *SPA) *Server {
	return &Server{
		cfg:      cfg,
		queue:    q,
		catalog:  cat,
		identity: ids,
		spa:      spa,
		trusted:  limit.ParseCIDRs(cfg.TrustedProxy),
		limiter:  limit.New(cfg.RatePerMin, cfg.RateBurst),
		remote:   newRemoteSizer(),
		oidc:     newCILogon(cfg),
	}
}

// Routes builds the handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Ops endpoints are never token-gated: a readiness probe cannot hold a secret.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /version", s.handleVersion)

	// External sign-in is a browser redirect flow, not an API call: it lands
	// here rather than under /api/v1 so a JSON error wrapper never intercepts a
	// 302 the provider needs to follow.
	if s.oidc != nil {
		mux.HandleFunc("GET /auth/cilogon", s.handleOIDCLogin)
		mux.HandleFunc("GET /auth/cilogon/callback", s.handleOIDCCallback)
	}

	v1 := http.NewServeMux()
	// Gated: ping is how a client checks that its credential works, which it
	// cannot do if an invalid one answers the same as a valid one.
	v1.Handle("GET /api/v1/ping", s.requireAuth(http.HandlerFunc(s.handlePing)))

	// Signing in has to work without being signed in, and the UI needs to know
	// who it is talking to before it can render anything — including "this
	// installation has no administrator yet".
	v1.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	v1.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	v1.HandleFunc("GET /api/v1/auth/me", s.handleMe)
	v1.Handle("POST /api/v1/auth/password", s.requireAuth(http.HandlerFunc(s.handleChangePassword)))

	// A caller's own API tokens. Gated on being someone, not on being an admin:
	// every account issues its own, and each carries that account's role.
	v1.Handle("GET /api/v1/auth/identities", s.requireAuth(http.HandlerFunc(s.handleListIdentities)))
	v1.Handle("GET /api/v1/auth/tokens", s.requireAuth(http.HandlerFunc(s.handleListTokens)))
	v1.Handle("POST /api/v1/auth/tokens", s.requireAuth(http.HandlerFunc(s.handleCreateToken)))
	v1.Handle("DELETE /api/v1/auth/tokens/{id}", s.requireAuth(http.HandlerFunc(s.handleRevokeToken)))

	// Catalog: what can be annotated, and what a snapshot pins.
	v1.Handle("GET /api/v1/snapshots", s.requireAuth(http.HandlerFunc(s.handleSnapshots)))
	v1.Handle("GET /api/v1/snapshots/{id}", s.requireAuth(http.HandlerFunc(s.handleSnapshot)))
	v1.Handle("GET /api/v1/sources", s.requireAuth(http.HandlerFunc(s.handleSources)))

	// Submission. Throttled per client IP, which the handler skips for a
	// token-bearing service account -- see trustedCaller.
	v1.Handle("POST /api/v1/annotate", s.requireAuth(s.throttle(http.HandlerFunc(s.handleAnnotate))))
	v1.Handle("POST /api/v1/annotate/vcf", s.requireAuth(s.throttle(http.HandlerFunc(s.handleAnnotateVCF))))

	// Jobs. Reads are ownership-enforced, not throttled.
	v1.Handle("GET /api/v1/jobs", s.requireAuth(http.HandlerFunc(s.handleListJobs)))
	v1.Handle("GET /api/v1/jobs/{id}", s.requireAuth(http.HandlerFunc(s.handleGetJob)))
	v1.Handle("GET /api/v1/jobs/{id}/log", s.requireAuth(http.HandlerFunc(s.handleJobLog)))
	v1.Handle("POST /api/v1/jobs/{id}/cancel", s.requireAuth(http.HandlerFunc(s.handleCancelJob)))
	v1.Handle("GET /api/v1/jobs/{id}/results", s.requireAuth(http.HandlerFunc(s.handleResults)))
	v1.Handle("GET /api/v1/jobs/{id}/export", s.requireAuth(http.HandlerFunc(s.handleExport)))

	// Administration is its own mux mounted behind one gate, so a route added
	// here cannot be left unguarded by forgetting to wrap it.
	v1.Handle("/api/v1/admin/", s.requireAdmin(s.adminRoutes()))

	mux.Handle("/api/v1/", s.withCaller(v1))

	// The SPA is mounted last and matches "/", so every route above wins. An
	// unknown /api path therefore 404s as JSON rather than returning the app
	// shell, which would be a confusing thing to debug from a client.
	if s.spa != nil {
		mux.Handle("/", s.spa.Handler())
	}

	return s.withCORS(logRequests(mux))
}

// adminRoutes is everything behind the administrator gate.
//
// Registered on its own mux rather than by prefix on the shared one so that
// adding a route here inherits the gate automatically — the previous shape left
// each route to remember for itself, and any valid token could administer.
func (s *Server) adminRoutes() http.Handler {
	m := http.NewServeMux()

	m.HandleFunc("POST /api/v1/admin/sources/validate", s.handleValidateSource)
	m.HandleFunc("POST /api/v1/admin/sources", s.handleCreateSource)
	m.HandleFunc("DELETE /api/v1/admin/sources/{id}", s.handleDeleteSource)
	m.HandleFunc("GET /api/v1/admin/sources/{id}/config", s.handleSourceConfig)
	m.HandleFunc("GET /api/v1/admin/sources/{id}/settings", s.handleSourceSettings)
	m.HandleFunc("PUT /api/v1/admin/sources/{id}/settings", s.handleSetSourceSettings)
	m.HandleFunc("GET /api/v1/admin/sources/{id}/grants", s.handleListGrants)
	m.HandleFunc("POST /api/v1/admin/sources/{id}/grants", s.handleGrant)
	m.HandleFunc("DELETE /api/v1/admin/sources/{id}/grants/{team}", s.handleRevokeGrant)
	m.HandleFunc("POST /api/v1/admin/snapshots", s.handleCreateSnapshot)
	m.HandleFunc("POST /api/v1/admin/snapshots/{id}/publish", s.handlePublishSnapshot)
	m.HandleFunc("PATCH /api/v1/admin/snapshots/{id}", s.handleUpdateSnapshotMeta)
	m.HandleFunc("PUT /api/v1/admin/snapshots/{id}/sources", s.handleSetSnapshotSources)
	m.HandleFunc("DELETE /api/v1/admin/snapshots/{id}", s.handleDeleteSnapshot)
	m.HandleFunc("GET /api/v1/admin/metrics", s.handleMetrics)
	m.HandleFunc("GET /api/v1/admin/storage", s.handleListStorage)
	m.HandleFunc("POST /api/v1/admin/storage", s.handleCreateStorage)
	m.HandleFunc("DELETE /api/v1/admin/storage/{id}", s.handleDeleteStorage)
	m.HandleFunc("GET /api/v1/admin/files", s.handleFiles)
	m.HandleFunc("POST /api/v1/admin/downloads", s.handleDownload)
	m.HandleFunc("POST /api/v1/admin/sources/{id}/default-reference", s.handleSetDefaultReference)
	m.HandleFunc("GET /api/v1/admin/registries", s.handleListRegistries)
	m.HandleFunc("POST /api/v1/admin/registries", s.handleCreateRegistry)
	m.HandleFunc("DELETE /api/v1/admin/registries/{id}", s.handleDeleteRegistry)
	m.HandleFunc("GET /api/v1/admin/registries/{id}/datasets", s.handleRegistryDatasets)
	m.HandleFunc("GET /api/v1/admin/registries/{id}/fetch", s.handleRegistryFetch)

	// Accounts and teams. Creating a user is also the bootstrap path: the
	// first-administrator credential passes requireAdmin and nothing else.
	m.HandleFunc("GET /api/v1/admin/users", s.handleListUsers)
	m.HandleFunc("POST /api/v1/admin/users", s.handleCreateUser)
	m.HandleFunc("PATCH /api/v1/admin/users/{id}", s.handleUpdateUser)
	m.HandleFunc("GET /api/v1/admin/teams", s.handleListTeams)
	m.HandleFunc("POST /api/v1/admin/teams", s.handleCreateTeam)
	m.HandleFunc("DELETE /api/v1/admin/teams/{id}", s.handleDeleteTeam)
	m.HandleFunc("POST /api/v1/admin/teams/{id}/members", s.handleAddMember)
	m.HandleFunc("DELETE /api/v1/admin/teams/{id}/members/{user}", s.handleRemoveMember)

	return s.requireIdentityStore(m)
}

// requireIdentityStore fails the account routes clearly when the database is
// unavailable, rather than panicking on a nil store.
func (s *Server) requireIdentityStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.identity == nil && isAccountRoute(r.URL.Path) {
			writeError(w, http.StatusServiceUnavailable, "accounts unavailable")
			return
		}
		h.ServeHTTP(w, r)
	})
}

func isAccountRoute(path string) bool {
	return strings.Contains(path, "/admin/users") ||
		strings.Contains(path, "/admin/teams") ||
		strings.Contains(path, "/grants")
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go s.limiterGC(ctx)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("api: listening on %s", s.cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

func (s *Server) limiterGC(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.limiter.GC(30 * time.Minute)
		}
	}
}

// throttle rate-limits by resolved client IP. Wraps submit routes only.
//
// See throttled: identified callers are exempt, anonymous ones are not.
func (s *Server) throttle(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.throttled(r) {
			h.ServeHTTP(w, r)
			return
		}
		if !s.limiter.Allow(limit.ClientIP(r, s.trusted)) {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded — slow down")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// withCORS allows the SPA's origin when one is configured. With no configured
// origins the header is omitted entirely, which is the correct default for a
// same-origin deployment.
func (s *Server) withCORS(h http.Handler) http.Handler {
	if len(s.cfg.CORSOrigins) == 0 {
		return h
	}
	allowed := map[string]bool{}
	for _, o := range s.cfg.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		log.Printf("api: %s %s %d %s", r.Method, r.URL.Path, sw.status,
			time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// --- handlers ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.queue.Ping(ctx); err != nil {
		// A readiness probe must fail when the database is unreachable, otherwise
		// the pod takes traffic it cannot serve.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "database",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
}

func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"pong": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
