package api

import (
	"io/fs"
	"net/http"
	"strings"
)

// SPA serves the built React app. A nil FS disables it, which is what an
// API-only deployment (or a dev run against `npm run dev`) wants.
//
// Kept out of the /api/v1 mux entirely: the SPA is public, and gating it behind
// the bearer token would mean the browser could never load the page that asks
// for the token.
type SPA struct {
	files fs.FS
}

// NewSPA wraps a filesystem holding the built assets.
func NewSPA(files fs.FS) *SPA { return &SPA{files: files} }

// Handler serves static assets and falls back to index.html for unknown paths,
// which is what client-side routing needs: a deep link like /jobs/abc123 has no
// file behind it, but must not 404 — the router resolves it in the browser.
//
// The fallback deliberately does NOT apply to /api, /healthz or /version; those
// are registered on the mux ahead of this handler and would otherwise return
// HTML for a missing endpoint, which is a confusing thing to debug.
func (s *SPA) Handler() http.Handler {
	fileServer := http.FileServer(http.FS(s.files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}

		if f, err := s.files.Open(p); err == nil {
			f.Close()
			// Hashed asset filenames are immutable, so they can be cached hard.
			// index.html must not be, or a deploy leaves clients on stale JS.
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// Unknown path: hand back the app shell and let the router decide.
		index, err := s.files.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		index.Close()
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}
