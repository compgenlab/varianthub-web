package api

import (
	"net/http"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// Visibility. A source is private unless published, and a private source is
// visible to an administrator and to the teams it has been granted to.
//
// The rule is applied to whole snapshots as well as to sources, and there it is
// *hiding*, not filtering: a snapshot pinning a source the caller cannot see is
// absent entirely rather than listed with a gap in it. Returning it minus the
// private source would produce annotations that silently lack fields the results
// claim to cover.

// visibility answers "may this caller see this source" for one request.
type visibility struct {
	admin   bool
	granted map[string]bool
}

// visibilityFor resolves the caller's grants once per request.
func (s *Server) visibilityFor(r *http.Request) (visibility, error) {
	c := callerOf(r)
	if c.IsAdmin() {
		return visibility{admin: true}, nil
	}
	if s.identity == nil || len(c.TeamIDs) == 0 {
		return visibility{}, nil
	}
	granted, err := s.identity.GrantedSourceIDs(r.Context(), c.TeamIDs)
	if err != nil {
		return visibility{}, err
	}
	return visibility{granted: granted}, nil
}

// canSee reports whether the source is visible to this caller.
func (v visibility) canSee(src catalog.Source) bool {
	if v.admin || src.Visibility != catalog.VisibilityPrivate {
		return true
	}
	return v.granted[src.ID]
}

// filterSources drops the sources this caller may not see.
func (v visibility) filterSources(in []catalog.Source) []catalog.Source {
	out := make([]catalog.Source, 0, len(in))
	for _, src := range in {
		if v.canSee(src) {
			out = append(out, src)
		}
	}
	return out
}

// canSeeSnapshot reports whether every source a snapshot pins is visible.
//
// All or nothing, deliberately. A snapshot is a claim about which annotations a
// result carries; handing back a version of it with a source quietly removed
// would answer a different question than the one asked, and the caller would
// have no way to tell.
func (v visibility) canSeeSnapshot(snap catalog.Snapshot) bool {
	if v.admin {
		return true
	}
	for _, src := range snap.Sources {
		if !v.canSee(src) {
			return false
		}
	}
	return true
}

// hiddenSources names the sources in a selection this caller may not see, so a
// refusal can say which — for a source the caller *can* see, being told which
// one is wrong is more useful than a blanket denial.
func (v visibility) hiddenSources(in []catalog.Source) []string {
	var out []string
	for _, src := range in {
		if !v.canSee(src) {
			out = append(out, src.Ref())
		}
	}
	return out
}
