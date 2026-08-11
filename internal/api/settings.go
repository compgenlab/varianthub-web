package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/jackc/pgx/v5/pgxpool"
)

// handleSiteSettings returns the deployment's settings three ways: as
// configured, as overridden, and as they actually apply.
//
// All three, because "why is this on?" is the question an administrator arrives
// with, and the effective value alone cannot answer it. Seeing that a value is
// the file's or somebody's override is the difference between changing the right
// thing and changing a file that is being ignored.
func (s *Server) handleSiteSettings(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "no catalog configured")
		return
	}
	over, err := s.catalog.SiteSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read settings: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"defaults":  s.siteDefaults(),
		"overrides": over,
		"effective": s.site(r.Context()),
		// So the form can render what exists rather than a list of its own that
		// falls behind the server's.
		"keys": catalog.SettingKeys,
		// Whether a cache is reachable at all. With no database URL the toggle
		// would be a switch wired to nothing.
		"cache_available": s.cfg.DatabaseURL != "",
	})
}

// handleSetSiteSettings records overrides. An empty value clears one, so the
// form can hand back "use the configured default" without a second verb.
func (s *Server) handleSetSiteSettings(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "no catalog configured")
		return
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.catalog.PutSiteSettings(r.Context(), body); err != nil {
		// The store validates before writing anything, so a rejection here means
		// the caller's input, not a half-applied form.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"effective": s.site(r.Context())})
}

// handleClearCache empties the annotation cache.
//
// Destructive but not dangerous: every value in it can be recomputed from the
// sources, which is the property that makes a cache a cache. The cost is that
// the next run of each query is slow again.
func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DatabaseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "no database configured")
		return
	}
	if err := clearAnnotationCache(r.Context(), s.cfg.DatabaseURL); err != nil {
		writeError(w, http.StatusInternalServerError, "clear cache: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

// clearAnnotationCache truncates the annotation cache.
//
// The tables do not exist yet: the cache is moving into this service (see
// internal/anncache) and until that lands there is nothing to clear, so this
// no-ops rather than erroring. It is wired up now because the control belongs
// with the rest of the deployment settings, not because it has work to do.
//
// Existence is checked first because TRUNCATE has no IF EXISTS, and "nothing to
// clear" is success rather than an error about a missing relation.
//
// Only the parents are named: values and tool lines follow by CASCADE, the same
// foreign key that makes eviction remove whole units rather than half of one.
func clearAnnotationCache(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	var present []string
	for _, t := range []string{"cache_variant_source", "cache_tool_header", "cache_data_source"} {
		var reg *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, t).Scan(&reg); err != nil {
			return err
		}
		if reg != nil {
			present = append(present, t)
		}
	}
	if len(present) == 0 {
		return nil
	}
	_, err = pool.Exec(ctx,
		`TRUNCATE TABLE `+strings.Join(present, ", ")+` RESTART IDENTITY CASCADE`)
	return err
}
