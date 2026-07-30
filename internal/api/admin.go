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

// --- registries ---

type registryRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	regs, err := s.catalog.ListRegistries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"registries": regs})
}

func (s *Server) handleCreateRegistry(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req registryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.ID == "" {
		req.ID = slug(req.Name)
	}
	if err := s.catalog.PutRegistry(r.Context(), catalog.Registry{
		ID: req.ID, Name: req.Name, URL: strings.TrimSpace(req.URL),
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Fetch it once so a bad URL fails now, while the operator is looking at it,
	// rather than the first time someone tries to import from it.
	if _, err := catalog.FetchManifest(r.Context(), req.URL); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": req.ID, "name": req.Name, "url": req.URL,
			"warning": "saved, but the registry could not be read: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": req.ID, "name": req.Name, "url": req.URL})
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	if err := s.catalog.DeleteRegistry(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRegistryDatasets lists what a registry offers.
func (s *Server) handleRegistryDatasets(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	reg, err := s.catalog.GetRegistry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	m, err := catalog.FetchManifest(r.Context(), reg.URL)
	if err != nil {
		// The registry is a remote we do not control; an outage there is not a
		// fault of this server.
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Normalize empty to [] rather than null: a client should not have to guard
	// every list it iterates.
	if m.Sources == nil {
		m.Sources = []catalog.RegistryEntry{}
	}
	if m.Snapshots == nil {
		m.Snapshots = []catalog.RegistryEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"registry": reg, "sources": m.Sources, "snapshots": m.Snapshots,
	})
}

// handleRegistryFetch returns one entry's config TOML for review.
//
// Deliberately not a one-click import: the fragment is executed by varhub and can
// name build recipes and container images. It goes into the editor so someone
// reads it before registering.
func (s *Server) handleRegistryFetch(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	reg, err := s.catalog.GetRegistry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref is required (name or name:version)")
		return
	}
	m, err := catalog.FetchManifest(r.Context(), reg.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	entry, err := m.FindEntry(ref)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	body, err := catalog.FetchEntry(r.Context(), reg.URL, entry)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref": entry.Ref(), "entry": entry, "toml": body,
		"origin": "registry: " + reg.ID,
	})
}

// slug turns a display name into an id.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
