package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// Catalog administration: register sources, build snapshots.
//
// **On authorization.** The design gates this behind an admin role. There are no
// accounts or roles yet — the API has one shared bearer token — so every
// token-holder can administer the catalog. That is a deliberate, documented gap,
// not an oversight: inventing a role check against an identity system that does
// not exist would give the appearance of authorization without any.
//
// The routes still sit under /admin so the eventual role gate has one place to
// attach, and so an audit of "what can change state" is a grep.
//
// A registered manifest is executed by varhub — it can name build recipes and
// container images. Anyone who can register a source can therefore run code on a
// worker. Treat the token as an administrative credential until roles land.

// maxTOMLBody bounds a manifest. Real fragments are a few KB; the ceiling is
// there so a stray upload cannot be read into memory unbounded.
const maxTOMLBody = 1 << 20

type sourceRequest struct {
	TOML       string `json:"toml"`
	ID         string `json:"id,omitempty"`
	Title      string `json:"title,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	Origin     string `json:"origin,omitempty"`
}

// derive builds a catalog.Source from the request, applying overrides over the
// fields parsed out of the manifest.
func (req sourceRequest) derive() (catalog.Source, error) {
	if strings.TrimSpace(req.TOML) == "" {
		return catalog.Source{}, errors.New("toml is required")
	}
	src, err := catalog.SourceFromTOML(req.TOML)
	if err != nil {
		return catalog.Source{}, err
	}
	if req.ID != "" {
		src.ID = req.ID
	}
	if req.Title != "" {
		src.Title = req.Title
	}
	if req.Detail != "" {
		src.Detail = req.Detail
	}
	if req.Origin != "" {
		src.Origin = req.Origin
	}
	switch req.Visibility {
	case "", catalog.VisibilityPublic:
		src.Visibility = catalog.VisibilityPublic
	case catalog.VisibilityPrivate:
		src.Visibility = catalog.VisibilityPrivate
	default:
		return catalog.Source{}, errors.New(`visibility must be "public" or "private"`)
	}
	return src, nil
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxTOMLBody)).Decode(v)
}

// handleValidateSource parses a manifest and reports what it would register,
// without writing anything. Drives the editor's validity indicator, so a typo is
// caught while typing rather than on submit.
func (s *Server) handleValidateSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	src, err := req.derive()
	if err != nil {
		// 200 with valid=false, not 4xx: an invalid draft is the expected state
		// while someone is typing, and the client wants the message, not an error.
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "id": src.ID, "name": src.Name,
		"version": src.Version, "kind": src.Kind, "title": src.Title,
	})
}

// handleCreateSource registers (or updates) a source from its manifest.
func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	src, err := req.derive()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.catalog.PutSource(r.Context(), src); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": src.ID, "ref": src.Ref(), "kind": src.Kind,
		"visibility": src.Visibility,
	})
}

type snapshotRequest struct {
	ID          string   `json:"id"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Build       string   `json:"build"`
	Defaults    []string `json:"defaults,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Sources     []string `json:"sources"`
	Publish     bool     `json:"publish,omitempty"`
}

// handleCreateSnapshot creates or updates a snapshot and its source pins.
func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req snapshotRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if strings.TrimSpace(req.Build) == "" {
		writeError(w, http.StatusBadRequest, "build is required (the assembly, e.g. GRCh38)")
		return
	}
	if len(req.Sources) == 0 {
		writeError(w, http.StatusBadRequest,
			"at least one source is required: a snapshot with no sources cannot annotate")
		return
	}

	state := catalog.StateDraft
	if req.Publish {
		state = catalog.StatePublished
	}
	if err := s.catalog.PutSnapshot(r.Context(), catalog.Snapshot{
		ID: req.ID, Title: req.Title, Description: req.Description,
		Build: req.Build, State: state,
		Defaults: req.Defaults, Tags: req.Tags,
	}, req.Sources); err != nil {
		// An unknown source id is a client mistake, not a server fault.
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Read it back: this is what proves the pins resolved and the manifest will
	// materialize. Without it a bad snapshot looks accepted and fails at job time.
	full, err := s.catalog.GetSnapshot(r.Context(), req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"snapshot written but does not load: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// handlePublishSnapshot flips a draft to published.
func (s *Server) handlePublishSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	id := r.PathValue("id")
	snap, err := s.catalog.GetSnapshot(r.Context(), id)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown snapshot")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ids := make([]string, 0, len(snap.Sources))
	for _, src := range snap.Sources {
		ids = append(ids, src.ID)
	}
	snap.State = catalog.StatePublished
	if err := s.catalog.PutSnapshot(r.Context(), snap, ids); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": catalog.StatePublished})
}
