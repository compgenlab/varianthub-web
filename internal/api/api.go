// Package api is the HTTP surface.
//
// Ops endpoints (/healthz, /version) are open; everything under /api/v1 is
// bearer-gated when VHW_REQUIRE_TOKEN is set. The v1 handlers live in v1.go.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/compgenlab/varianthub-web/internal/auth"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/limit"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Server is the API server.
type Server struct {
	cfg     *config.Config
	queue   *queue.Queue
	catalog *catalog.Store // nil disables the catalog endpoints
	spa     *SPA           // nil serves no web UI (API-only)
	trusted []*net.IPNet
	limiter *limit.Limiter
	remote  *remoteSizer
}

// New builds the server. cat may be nil, in which case the catalog endpoints
// report 503 rather than the process refusing to start -- annotation submission
// and job polling do not need the catalog.
func New(cfg *config.Config, q *queue.Queue, cat *catalog.Store, spa *SPA) *Server {
	return &Server{
		cfg:     cfg,
		queue:   q,
		catalog: cat,
		spa:     spa,
		trusted: limit.ParseCIDRs(cfg.TrustedProxy),
		limiter: limit.New(cfg.RatePerMin, cfg.RateBurst),
		remote:  newRemoteSizer(),
	}
}

// Routes builds the handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Ops endpoints are never token-gated: a readiness probe cannot hold a secret.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /version", s.handleVersion)

	v1 := http.NewServeMux()
	v1.HandleFunc("GET /api/v1/ping", s.handlePing)

	// Catalog: what can be annotated, and what a snapshot pins.
	v1.HandleFunc("GET /api/v1/snapshots", s.handleSnapshots)
	v1.HandleFunc("GET /api/v1/snapshots/{id}", s.handleSnapshot)
	v1.HandleFunc("GET /api/v1/sources", s.handleSources)

	// Submission. Throttled per client IP, which the handler skips for a
	// token-bearing service account -- see trustedCaller.
	v1.Handle("POST /api/v1/annotate", s.throttle(http.HandlerFunc(s.handleAnnotate)))
	v1.Handle("POST /api/v1/annotate/vcf", s.throttle(http.HandlerFunc(s.handleAnnotateVCF)))

	// Catalog administration. Under /admin so the eventual role gate has one
	// place to attach; today any valid token can administer (see admin.go).
	v1.HandleFunc("POST /api/v1/admin/sources/validate", s.handleValidateSource)
	v1.HandleFunc("POST /api/v1/admin/sources", s.handleCreateSource)
	v1.HandleFunc("DELETE /api/v1/admin/sources/{id}", s.handleDeleteSource)
	v1.HandleFunc("GET /api/v1/admin/sources/{id}/config", s.handleSourceConfig)
	v1.HandleFunc("POST /api/v1/admin/snapshots", s.handleCreateSnapshot)
	v1.HandleFunc("POST /api/v1/admin/snapshots/{id}/publish", s.handlePublishSnapshot)
	v1.HandleFunc("PATCH /api/v1/admin/snapshots/{id}", s.handleUpdateSnapshotMeta)
	v1.HandleFunc("PUT /api/v1/admin/snapshots/{id}/sources", s.handleSetSnapshotSources)
	v1.HandleFunc("DELETE /api/v1/admin/snapshots/{id}", s.handleDeleteSnapshot)
	v1.HandleFunc("GET /api/v1/admin/metrics", s.handleMetrics)
	v1.HandleFunc("GET /api/v1/admin/storage", s.handleListStorage)
	v1.HandleFunc("POST /api/v1/admin/storage", s.handleCreateStorage)
	v1.HandleFunc("DELETE /api/v1/admin/storage/{id}", s.handleDeleteStorage)
	v1.HandleFunc("GET /api/v1/admin/files", s.handleFiles)
	v1.HandleFunc("POST /api/v1/admin/downloads", s.handleDownload)
	v1.HandleFunc("GET /api/v1/admin/registries", s.handleListRegistries)
	v1.HandleFunc("POST /api/v1/admin/registries", s.handleCreateRegistry)
	v1.HandleFunc("DELETE /api/v1/admin/registries/{id}", s.handleDeleteRegistry)
	v1.HandleFunc("GET /api/v1/admin/registries/{id}/datasets", s.handleRegistryDatasets)
	v1.HandleFunc("GET /api/v1/admin/registries/{id}/fetch", s.handleRegistryFetch)

	// Jobs. Reads are ownership-enforced, not throttled.
	v1.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	v1.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)
	v1.HandleFunc("GET /api/v1/jobs/{id}/results", s.handleResults)
	v1.HandleFunc("GET /api/v1/jobs/{id}/export", s.handleExport)

	var v1h http.Handler = v1
	if s.cfg.RequireToken {
		v1h = auth.RequireToken(s.cfg.MasterKey, v1h)
	}
	mux.Handle("/api/v1/", v1h)

	// The SPA is mounted last and matches "/", so every route above wins. An
	// unknown /api path therefore 404s as JSON rather than returning the app
	// shell, which would be a confusing thing to debug from a client.
	if s.spa != nil {
		mux.Handle("/", s.spa.Handler())
	}

	return s.withCORS(logRequests(mux))
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
// A token-bearing service account is exempt: the limit exists to stop an
// anonymous browser flooding the queue, and applying it to a bulk ingest would
// make that ingest throttle itself.
func (s *Server) throttle(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.trustedCaller(r) {
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
