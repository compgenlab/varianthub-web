// Package webui embeds the built React app so the server ships as one binary.
//
// dist/ is a build artifact, not source: `npm --prefix web run build` produces
// it. The placeholder index.html is committed so `go build` works on a fresh
// checkout without node installed — an API-only build is still useful, and a
// missing embed directory is a compile error rather than a runtime one.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built assets rooted at dist/, or nil if only the placeholder
// is present (no real build has been made).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
