package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/queue"
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
	// Assets are the helper files a build recipe or tool step names. They come
	// back from a registry fetch and are posted here with the manifest, so what
	// gets stored is what was reviewed — the fetch does not import anything on
	// its own.
	//
	// A pointer, so "not mentioned" is distinct from "none". Re-registering a
	// manifest to change one line used to arrive with no assets and replace the
	// stored set with nothing, silently deleting the scripts a tool cannot run
	// without — the failure then appears at the next annotation as a missing
	// file, with nothing connecting it to the edit. Absent now means keep what
	// is there; an explicit [] still clears.
	Assets *[]catalog.Asset `json:"assets,omitempty"`
	// Settings this deployment applies to the source, as opposed to what the
	// manifest says about itself. Accepted at registration so a prefix can be
	// chosen when a second version of something is added, which is when the
	// need for one becomes obvious.
	Settings *catalog.SourceSettings `json:"settings,omitempty"`
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
	// Left empty when the request says nothing, so "not mentioned" stays distinct
	// from "make it closed".
	//
	// It used to default to restricted right here, which is correct for
	// registration and wrong for an edit: the manifest editor posts only the TOML,
	// so saving an unrelated one-line change to a public source silently made it
	// invisible to everyone who was using it. The two callers want different
	// things from silence, so neither is decided here — registration falls through
	// to the store's closed default, and an update carries the stored value
	// forward.
	switch req.Visibility {
	case "":
		src.Visibility = ""
	case "private":
		// What restricted used to be called. Accepted so a script written against
		// the old name keeps working, and meaning exactly what it did before.
		src.Visibility = catalog.VisibilityRestricted
	default:
		if !catalog.ValidVisibility(req.Visibility) {
			return catalog.Source{}, errors.New(
				`visibility must be "public", "signed_in" or "restricted"`)
		}
		src.Visibility = req.Visibility
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
		// Whether registering this will leave something still to do. The
		// register form asks where the data should go *before* writing the
		// manifest, so it has to know from the draft rather than after the fact
		// — a source registered and then forgotten is one that fails at
		// annotate time with "sources not downloaded".
		"needs_data": src.NeedsData(),
		"stream":     src.Stream,
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
	// Read before the write, so a re-registration that says nothing about assets
	// keeps the ones already stored.
	var existingAssets []catalog.Asset
	if prev, err := s.catalog.Assets(r.Context(), src.ID); err == nil {
		existingAssets = prev
	}

	if err := s.catalog.PutSource(r.Context(), src); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Written after the source, because the rows reference it. Replacing the set
	// keeps the stored files in step with the manifest that names them — but
	// only when the caller said something about them.
	assets := existingAssets
	if req.Assets != nil {
		assets = *req.Assets
		if err := s.catalog.PutAssets(r.Context(), src.ID, assets); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Settings != nil {
		if err := s.catalog.PutSettings(r.Context(), src.ID, *req.Settings); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// Anything the manifest names but nobody supplied. Reported rather than
	// refused: a hand-written manifest is a legitimate way to register a source,
	// and the files can be added later — but the caller should learn now, not
	// from a download that fails at the first recipe step.
	missing := catalog.MissingAssets(src.TOML, assets)

	writeJSON(w, http.StatusOK, map[string]any{
		"id": src.ID, "ref": src.Ref(), "kind": src.Kind,
		"visibility": src.Visibility, "assets": len(assets),
		"missing_assets": missing,
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
		// Changing a published snapshot's pins is refused, not a failure: 409
		// says the request conflicts with the resource's state.
		if errors.Is(err, catalog.ErrPinsFrozen) {
			writeError(w, http.StatusConflict, err.Error()+
				" — edit its title or defaults instead, or create a new snapshot")
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
	// The helper files the fragment names, fetched with it. Returned rather
	// than imported: the review-before-registering rule applies to a script a
	// build step will execute at least as much as it does to the manifest.
	assets, err := catalog.FetchEntryAssets(r.Context(), reg.URL, entry, body)
	if err != nil {
		// The manifest is still useful without them, and saying which asset
		// could not be fetched beats failing the whole fetch.
		writeJSON(w, http.StatusOK, map[string]any{
			"ref": entry.Ref(), "entry": entry, "toml": body,
			"origin": "registry: " + reg.ID,
			"assets": []catalog.Asset{}, "asset_error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref": entry.Ref(), "entry": entry, "toml": body,
		"origin": "registry: " + reg.ID,
		"assets": assets,
	})
}

// handleSourceSettings reads a source's deployment-local settings.
func (s *Server) handleSourceSettings(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	id := r.PathValue("id")
	src, err := s.catalog.GetSource(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such source")
		return
	}
	set, err := s.catalog.Settings(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": set,
		// What the manifest itself declares, so the UI can show what a blank
		// override falls back to rather than implying there is no prefix.
		"manifest_prefix": catalog.ManifestPrefix(src.TOML),
		// Only a tool provisioned to an object store can publish its setup, so
		// the UI can say why the control does nothing rather than offering it.
		"is_tool": src.Kind == "tool",
	})
}

// handleSetSourceSettings replaces them.
func (s *Server) handleSetSourceSettings(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	id := r.PathValue("id")
	if _, err := s.catalog.GetSource(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "no such source")
		return
	}
	var set catalog.SourceSettings
	if err := decodeJSON(r, &set); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := s.catalog.PutSettings(r.Context(), id, set); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("api: settings for source %s changed by %s", id, callerOf(r).Label())
	writeJSON(w, http.StatusOK, map[string]any{"settings": set})
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

// handleUpdateSnapshotMeta edits a snapshot's presentation and default fields.
//
// Allowed on a published snapshot: publishing fixes the pinned source versions,
// which is what reproducibility depends on. A title or a default field selection
// is a convenience, and a job records what it actually ran with anyway — so
// freezing those too would only mean a typo could never be corrected.
func (s *Server) handleUpdateSnapshotMeta(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req struct {
		Title       *string  `json:"title"`
		Description *string  `json:"description"`
		Defaults    []string `json:"defaults"`
		Tags        []string `json:"tags"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	id := r.PathValue("id")
	cur, err := s.catalog.GetSnapshot(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Absent fields keep their value; an explicit empty list clears one.
	if req.Title != nil {
		cur.Title = *req.Title
	}
	if req.Description != nil {
		cur.Description = *req.Description
	}
	if req.Defaults != nil {
		cur.Defaults = req.Defaults
	}
	if req.Tags != nil {
		cur.Tags = req.Tags
	}
	if err := s.catalog.UpdateSnapshotMeta(r.Context(), cur); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cur)
}

// handleDeleteSnapshot removes a snapshot, published or not.
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	if err := s.catalog.DeleteSnapshot(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- storage locations and downloads ---

func (s *Server) handleListStorage(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	locs, err := s.catalog.ListStorage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		catalog.StorageLocation
		Usable bool   `json:"usable"`
		Reason string `json:"unusable_reason,omitempty"`
	}
	out := make([]item, 0, len(locs))
	for _, l := range locs {
		out = append(out, item{StorageLocation: l, Usable: l.Usable(), Reason: l.UnusableReason()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"storage": out})
}

func (s *Server) handleCreateStorage(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
		URI  string `json:"uri"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.ID == "" {
		req.ID = slug(req.Name)
	}
	// Filesystem locations are deliberately not addable here: a path only means
	// something if the worker has it mounted, which is a deployment decision. The
	// config file declares those; the API takes S3 buckets, which need no mount.
	if req.Kind != catalog.StorageS3 {
		writeError(w, http.StatusBadRequest,
			"only S3 locations can be added here — filesystem paths must be declared "+
				"in the deployment config (VHW_STORAGE_PATHS), since the worker has to mount them")
		return
	}
	if err := s.catalog.PutStorage(r.Context(), catalog.StorageLocation{
		ID: req.ID, Name: req.Name, Kind: req.Kind, URI: req.URI,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": req.ID, "name": req.Name, "uri": req.URI})
}

func (s *Server) handleDeleteStorage(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	if err := s.catalog.DeleteStorage(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFiles lists downloaded files, optionally for one source.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	q := r.URL.Query()
	files, err := s.catalog.SourceFiles(r.Context(), q.Get("source"), q.Get("storage"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files, "total_bytes": total, "count": len(files),
	})
}

// handleDownload queues a provisioning job for a set of sources.
//
// Sources, not snapshots: a source is the unit of data. Requiring one to belong
// to a snapshot first would mean a newly registered source could not be
// downloaded until someone bundled it, which is backwards — you bundle sources
// you already have.
//
// It goes through the same queue as annotation rather than running inline: a
// download can take hours and move gigabytes, which is not something to hold an
// HTTP request open for, and reusing the queue means it gets the same
// persistence, progress and error reporting.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req struct {
		Sources   []string `json:"sources"`
		StorageID string   `json:"storage_id"`
		Force     bool     `json:"force"`

		// IncludeStreamed downloads sources that ask to be streamed. Off by
		// default so the common case does not accidentally pull tens of
		// gigabytes, but available because `stream` is the publisher's
		// suggestion and not a policy.
		IncludeStreamed bool `json:"include_streamed"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Sources) == 0 {
		writeError(w, http.StatusBadRequest, "`sources` is required — select at least one source")
		return
	}

	var loc catalog.StorageLocation
	var err error
	if req.StorageID != "" {
		loc, err = s.catalog.GetStorage(r.Context(), req.StorageID)
	} else {
		loc, err = s.catalog.DefaultStorage(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !loc.Usable() {
		writeError(w, http.StatusBadRequest, loc.UnusableReason())
		return
	}

	// Validate the ids here so an unknown source is a 400 now, rather than a job
	// that fails a minute later in a worker log.
	all, err := s.catalog.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	known := map[string]catalog.Source{}
	for _, src := range all {
		known[src.ID] = src
	}
	labels := make([]string, 0, len(req.Sources))
	wanted := make([]string, 0, len(req.Sources))
	var skipped []string
	for _, id := range req.Sources {
		src, ok := known[id]
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown source "+strconv.Quote(id))
			return
		}
		// Some sources have nothing to provision: a builtin computes from the
		// variant, and a streamed source is read from its url. Silently
		// including one would queue a job that downloads nothing and reports
		// success, which looks like it did something.
		//
		// A streamed source is the exception the caller can ask for: it does
		// have data, it just is not normally copied.
		if !src.NeedsData() && !(src.Stream && req.IncludeStreamed) {
			skipped = append(skipped, src.Ref())
			continue
		}
		wanted = append(wanted, id)
		labels = append(labels, src.Ref())
	}
	if len(wanted) == 0 {
		writeError(w, http.StatusBadRequest, "nothing to download: "+
			strings.Join(skipped, ", ")+" have no data to fetch — a builtin computes "+
			"from the variant, and a streamed source is read from its url "+
			"(pass include_streamed to download a streamed source anyway)")
		return
	}
	req.Sources = wanted

	body, err := json.Marshal(map[string]any{
		"storage_id": loc.ID, "cache_dir": loc.URI,
		"sources": req.Sources, "force": req.Force,
		"no_stream": req.IncludeStreamed,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	label := strings.Join(labels, ", ")
	if len(label) > 80 {
		label = fmt.Sprintf("%d sources", len(labels))
	}
	id, err := s.queue.Enqueue(r.Context(), queue.NewChunk{
		Kind:    queue.KindDownload,
		Session: sessionOf(r),
		UserID:  callerOf(r).UserID(),
		Label:   "download " + label + " → " + loc.Name,
		// Which sources this job is for, so the catalog can ask "is anything
		// fetching this one" without opening every job's body. A tool records
		// no files — its image and data go to the worker's local data_dir, not
		// to the storage location — so job state is the only evidence that one
		// has been installed at all.
		Selection: strings.Join(req.Sources, ","),
		// Heavier than an annotation: this saturates disk and CPU for as long as
		// it runs, so it holds more of the pool while it does.
		Weight: s.cfg.DownloadWeight,
		Body:   body,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id": id, "sources": req.Sources, "storage": loc,
	})
}

// handleDeleteSource removes a source no snapshot pins, and queues the reclaim
// of its files.
//
// Refused while pinned rather than cascaded: removing it would silently change
// what those snapshots mean, and a published snapshot is a promise that its
// pinned versions do not move. The error names the snapshots so the caller knows
// what to detach.
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	id := r.PathValue("id")
	src, locations, err := s.catalog.DeleteSource(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrSourcePinned):
			// 409: the request conflicts with the resource's state, and the fix is
			// to change that state rather than to retry.
			writeError(w, http.StatusConflict, err.Error()+
				" — remove it from those snapshots first")
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// The row is gone; the bytes are not. Only the worker mounts the storage, so
	// reclaiming them is a job — one per location the source occupied.
	jobs := []string{}
	for _, loc := range locations {
		if loc.Kind != catalog.StoragePath {
			continue // nothing to remove for a location we never wrote to
		}
		body, mErr := json.Marshal(map[string]any{
			"root": loc.URI, "name": src.Name, "version": src.Version,
		})
		if mErr != nil {
			continue
		}
		jobID, qErr := s.queue.Enqueue(r.Context(), queue.NewChunk{
			Kind:    queue.KindCleanup,
			Session: sessionOf(r),
			UserID:  callerOf(r).UserID(),
			Label:   "remove " + src.Ref() + " from " + loc.Name,
			Body:    body,
		})
		if qErr != nil {
			// The source is already deleted; failing the response now would imply
			// otherwise. Report the orphan instead.
			log.Printf("api: source %s deleted but cleanup could not be queued for %s: %v",
				src.Ref(), loc.Name, qErr)
			continue
		}
		jobs = append(jobs, jobID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "ref": src.Ref(), "cleanup_jobs": jobs,
	})
}

// handleSourceConfig returns a source's stored manifest.
//
// The manifest is the source of truth — the columns beside it are a derived
// projection — so being able to read it back is how an admin checks what a
// source actually declares, rather than inferring it from the listing.
func (s *Server) handleSourceConfig(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	src, err := s.catalog.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": src.ID, "ref": src.Ref(), "format": "toml", "config": src.TOML,
	})
}

// handleSetSnapshotSources replaces a draft snapshot's source set.
func (s *Server) handleSetSnapshotSources(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req struct {
		Sources []string `json:"sources"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	snap, err := s.catalog.SetSnapshotSources(r.Context(), r.PathValue("id"), req.Sources)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, catalog.ErrPinsFrozen):
		// 409 rather than 403: the request is well-formed and the caller is
		// permitted; the snapshot's state is what refuses it.
		writeError(w, http.StatusConflict,
			"a published snapshot's sources are fixed — duplicate it to change them")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": snap, "annotations": snapshotAnnotations(snap),
	})
}

// handleSetDefaultReference marks a reference genome as the one ad-hoc
// snapshots pin for its assembly.
//
// Ad-hoc annotation assembles a snapshot per job from whatever the caller
// selected, so it has nobody to ask which genome to use. This is that answer.
// The chosen source is still pinned into the snapshot, so the default is a
// decision made once rather than an indirection resolved on every run.
func (s *Server) handleSetDefaultReference(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	if err := s.catalog.SetDefaultReference(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMoveSource relocates a source's files to another storage location.
//
// Queued rather than done inline: it moves the same volume of data a download
// does — tens of gigabytes for a large source — and only the worker can reach
// both ends.
func (s *Server) handleMoveSource(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil || s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req struct {
		StorageID string `json:"storage_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	sourceID := r.PathValue("id")
	src, err := s.catalog.GetSource(r.Context(), sourceID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	to, err := s.catalog.GetStorage(r.Context(), req.StorageID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !to.Usable() {
		writeError(w, http.StatusBadRequest, to.UnusableReason())
		return
	}

	// Which location it is leaving is derived rather than asked for: a caller
	// naming the wrong origin would move nothing and report success.
	locs, err := s.catalog.StorageForSource(r.Context(), sourceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var from catalog.StorageLocation
	for _, l := range locs {
		if l.ID != req.StorageID {
			from = l
			break
		}
	}
	if from.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"%s has no files to move (it is already in %s, or has not been downloaded)",
			src.Ref(), to.Name))
		return
	}

	body, err := json.Marshal(map[string]any{
		"source_id": sourceID, "from_storage": from.ID, "to_storage": to.ID,
		"from_uri": from.URI, "to_uri": to.URI,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := s.queue.Enqueue(r.Context(), queue.NewChunk{
		Kind:      queue.KindMove,
		Session:   sessionOf(r),
		UserID:    callerOf(r).UserID(),
		Label:     "move " + src.Ref() + ": " + from.Name + " → " + to.Name,
		Selection: sourceID,
		// Same volume of data as fetching it, so the same cost to the pool.
		Weight: s.cfg.DownloadWeight,
		Body:   body,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id": id, "from": from.Name, "to": to.Name,
	})
}

// handleListBuilds lists the genome builds this installation offers.
//
// Not admin-gated: the annotation form needs it to populate its picker and to
// filter sources, and which assemblies exist is not sensitive — it is visible
// from the source list either way.
func (s *Server) handleListBuilds(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	builds, err := s.catalog.ListBuilds(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, BuildsResponse{Builds: builds})
}

// handlePutBuild adds or updates a build.
func (s *Server) handlePutBuild(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var b catalog.Build
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := s.catalog.PutBuild(r.Context(), b); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": strings.TrimSpace(b.Name)})
}

// handleDeleteBuild removes a build, refusing while it is still in use.
func (s *Server) handleDeleteBuild(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	if err := s.catalog.DeleteBuild(r.Context(), r.PathValue("name")); err != nil {
		// 409: the request is well-formed and the caller may retry it after
		// moving what depends on it, which is not a 400.
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCheckStorage probes every storage location and reports which answer.
//
// Exists because the alternative is finding out from a job. An object store
// that stopped listening surfaced as a provisioning failure whose message was an
// SDK retry trace, hours into a run — while the page that lists storage had no
// opinion about whether any of it was reachable.
//
// Probed on request rather than polled: the check costs a round trip per
// location, and an operator asking "is the store up?" is the moment the answer
// needs to be current rather than cached.
func (s *Server) handleCheckStorage(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	locs, err := s.catalog.ListStorage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type result struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
		URI  string `json:"uri"`
		blob.Health
	}
	// Concurrently: an unreachable endpoint costs the full timeout, and several
	// of those in series is a page that appears to hang.
	out := make([]result, len(locs))
	var wg sync.WaitGroup
	for i, l := range locs {
		wg.Add(1)
		go func(i int, l catalog.StorageLocation) {
			defer wg.Done()
			out[i] = result{
				ID: l.ID, Name: l.Name, Kind: string(l.Kind), URI: l.URI,
				Health: blob.Check(r.Context(), l.URI),
			}
		}(i, l)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"locations": out})
}

// handleUpdateSource replaces a source's manifest without re-provisioning it.
//
// Registering again through POST would work for the manifest itself, but takes
// index_status from the new draft — so a correction to one line would mark a
// source as not downloaded and cost a re-fetch. For VEP that is hours, for a
// missing requires_reference it is absurd.
func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	next, err := req.derive()
	if err != nil {
		// A manifest that does not parse is the common case here — this is a
		// text box — so it is a 400 with the parser's own words.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.catalog.UpdateSourceTOML(r.Context(), r.PathValue("id"), next)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			// Pinned by a snapshot, or renamed: both are the caller's to fix and
			// both are explained in the message.
			writeError(w, http.StatusConflict, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": updated})
}
