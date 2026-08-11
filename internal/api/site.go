package api

import (
	"context"
	"net/http"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// siteDefaults is the deployment as configured, before any stored override.
func (s *Server) siteDefaults() catalog.Site { return catalog.SiteFromConfig(s.cfg) }

// site resolves the deployment's effective settings: the configured defaults
// with any administrator override applied.
//
// Every read of a settable value goes through here rather than through s.cfg,
// which holds only what the file said at startup. Reading s.cfg directly is the
// bug this exists to prevent: the form saves, the database has the new value,
// and the running server keeps answering with the old one.
//
// A catalog that is unreachable falls back to the configured defaults. Refusing
// to serve because a settings table could not be read would take an installation
// down over something it has a perfectly good answer for.
func (s *Server) site(ctx context.Context) catalog.Site {
	if s.catalog == nil {
		return s.siteDefaults()
	}
	out, err := s.catalog.EffectiveSite(ctx, s.siteDefaults())
	if err != nil {
		return s.siteDefaults()
	}
	return out
}

// allowAnonymous is the effective answer for one request.
func (s *Server) allowAnonymous(r *http.Request) bool {
	return s.site(r.Context()).AllowAnonymous
}
