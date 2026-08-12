package api

import (
	"net/http"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// Visibility. Three levels, from least to most restrictive:
//
//	public      anyone who can reach the server, anonymous visitors included
//	signed_in   any account, with no grant needed
//	restricted  membership of a team the source is granted to
//
// The rule is applied to whole snapshots as well as to sources, and there it is
// *hiding*, not filtering: a snapshot pinning a source the caller cannot see is
// absent entirely rather than listed with a gap in it. Returning it minus the
// hidden source would produce annotations that silently lack fields the results
// claim to cover.

// visibility answers "may this caller see this source" for one request.
type visibility struct {
	admin bool
	// signedIn is whether the caller holds an account, as opposed to an
	// anonymous session. It is the whole of the signed_in level.
	signedIn bool
	granted  map[string]bool
}

// visibilityFor resolves the caller's standing once per request.
func (s *Server) visibilityFor(r *http.Request) (visibility, error) {
	c := callerOf(r)
	if c.IsAdmin() {
		return visibility{admin: true, signedIn: true}, nil
	}
	// An anonymous session is a caller the server issued an identity to, not one
	// that has an account — the distinction the signed_in level is made of. A
	// token belongs to a user, so it counts; AnonSession alone does not.
	in := c.User != nil
	if s.identity == nil || len(c.TeamIDs) == 0 {
		return visibility{signedIn: in}, nil
	}
	granted, err := s.identity.GrantedSourceIDs(r.Context(), c.TeamIDs)
	if err != nil {
		return visibility{}, err
	}
	return visibility{signedIn: in, granted: granted}, nil
}

// allows reports whether this caller clears a level, ignoring per-source grants.
func (v visibility) allows(level string) bool {
	switch level {
	case catalog.VisibilityPublic:
		return true
	case catalog.VisibilitySignedIn:
		return v.signedIn
	case catalog.VisibilityRestricted:
		return false // decided per source, by grant
	default:
		// A level this code does not understand is not one it hands data out on.
		// The database constrains the column, so reaching here means the schema
		// moved ahead of the binary — during a rollout, say — and refusing is the
		// only safe reading.
		return false
	}
}

// canSee reports whether the source is visible to this caller.
func (v visibility) canSee(src catalog.Source) bool {
	if v.admin {
		return true
	}
	if v.allows(src.Visibility) {
		return true
	}
	// Only restricted consults grants, but granting a source that is merely
	// signed_in is harmless and should still work — a grant is a stronger
	// statement than the level it is attached to.
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

// canSeeSnapshot reports whether the caller can see every source a snapshot pins.
//
// All or nothing, deliberately. A snapshot is a claim about which annotations a
// result carries; handing back a version of it with a source quietly removed
// would answer a different question than the one asked, and the caller would have
// no way to tell.
//
// A snapshot has no level of its own to check. It cannot be offered more widely
// than its sources without promising annotations the caller may not compute, and
// it cannot be narrowed further without a second place for an access decision to
// live — one that could only agree with the sources or contradict them.
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
// refusal can say which — for a source the caller *can* see, being told which one
// is wrong is more useful than a blanket denial.
func (v visibility) hiddenSources(in []catalog.Source) []string {
	var out []string
	for _, src := range in {
		if !v.canSee(src) {
			out = append(out, src.Ref())
		}
	}
	return out
}
