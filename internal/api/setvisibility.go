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

// There is deliberately no snapshot equivalent. A snapshot's level follows from
// what it pins, so the way to change it is to change a source's level or to pin
// different sources. Offering a setting here would put an access decision in two
// places, where the second can only agree with the first or be quietly wrong.
//
// snapshotConstraints names the pinned sources holding a snapshot above public,
// so a listing can say *why* it is not offered to everyone rather than only that
// it is not.
func snapshotConstraints(snap catalog.Snapshot) []string {
	var by []string
	for _, src := range snap.Sources {
		if src.Visibility != catalog.VisibilityPublic {
			by = append(by, src.Ref()+" ("+src.Visibility+")")
		}
	}
	return by
}
