package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/catalog"
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

// clearAnnotationCache empties the annotation cache.
//
// Its own connection rather than the catalog's pool, because the cache is not
// part of the catalog: it is a separate store that happens to share a database,
// and a deployment could move it to another one without this handler changing.
func clearAnnotationCache(ctx context.Context, dsn string) error {
	store, err := anncache.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.Clear(ctx)
}
