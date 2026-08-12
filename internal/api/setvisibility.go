package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// Changing who may use a source or a snapshot.
//
// Its own endpoint rather than a field on the update handlers, because the two
// are different kinds of change and one of them is an access decision. Editing a
// manifest is about what a source *is*; this is about who it is for. Folding the
// second into the first is what made saving an unrelated one-line edit close a
// public source to everyone using it.

type visibilityRequest struct {
	Visibility string `json:"visibility"`
}

// parse validates the requested level, accepting the name restricted used to
// have so an existing script keeps working.
func (req visibilityRequest) parse() (string, error) {
	v := strings.ToLower(strings.TrimSpace(req.Visibility))
	if v == "private" {
		v = catalog.VisibilityRestricted
	}
	if !catalog.ValidVisibility(v) {
		return "", fmt.Errorf("visibility %q: want %q (anyone, including anonymous), "+
			"%q (any account) or %q (a team grant)",
			req.Visibility, catalog.VisibilityPublic, catalog.VisibilitySignedIn,
			catalog.VisibilityRestricted)
	}
	return v, nil
}

// handleSetSourceVisibility changes who may use one source.
//
// Gene lists are sources, so this is their toggle too — there is nothing for the
// gene-list screen to do beyond calling it.
func (s *Server) handleSetSourceVisibility(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req visibilityRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	level, err := req.parse()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	src, err := s.catalog.SetSourceVisibility(r.Context(), r.PathValue("id"), level)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": src.ID, "ref": src.Ref(), "visibility": src.Visibility,
	})
}

// handleSetSnapshotVisibility changes who may use one snapshot.
//
// A snapshot's own level can only narrow what its sources already decide, so this
// reports the effective level alongside the stored one. Otherwise setting a
// snapshot to public and seeing nothing change would look like the setting was
// ignored, when what happened is that a pinned source is the binding constraint.
func (s *Server) handleSetSnapshotVisibility(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req visibilityRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	level, err := req.parse()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id := r.PathValue("id")
	if err := s.catalog.SetSnapshotVisibility(r.Context(), id, level); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-read with sources so the effective level is the real one.
	snap, err := s.catalog.GetSnapshot(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := map[string]any{
		"id":                   snap.ID,
		"visibility":           snap.Visibility,
		"effective_visibility": snap.EffectiveVisibility(),
	}
	// Name the sources doing the constraining, so "why is my public snapshot
	// still hidden" is answerable from the response rather than by checking each
	// pinned source by hand.
	if eff := snap.EffectiveVisibility(); eff != snap.Visibility {
		var by []string
		for _, src := range snap.Sources {
			if catalog.VisibilityRank(src.Visibility) > catalog.VisibilityRank(snap.Visibility) {
				by = append(by, src.Ref()+" ("+src.Visibility+")")
			}
		}
		out["constrained_by"] = by
		out["note"] = fmt.Sprintf(
			"stored as %s, but offered at %s because of the sources it pins", snap.Visibility, eff)
	}
	writeJSON(w, http.StatusOK, out)
}
