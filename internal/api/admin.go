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
		// Builtins compute from the variant and have nothing on disk. Silently
		// including one would queue a job that downloads nothing and reports
		// success, which looks like it did something.
		if !src.NeedsData() {
			skipped = append(skipped, src.Ref())
			continue
		}
		wanted = append(wanted, id)
		labels = append(labels, src.Ref())
	}
	if len(wanted) == 0 {
		writeError(w, http.StatusBadRequest, "nothing to download: "+
			strings.Join(skipped, ", ")+" compute from the variant and need no data")
		return
	}
	req.Sources = wanted

	body, err := json.Marshal(map[string]any{
		"storage_id": loc.ID, "cache_dir": loc.URI,
		"sources": req.Sources, "force": req.Force,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	label := strings.Join(labels, ", ")
	if len(label) > 80 {
		label = fmt.Sprintf("%d sources", len(labels))
	}
	id, err := s.queue.Enqueue(r.Context(), queue.NewJob{
		Kind:    queue.KindDownload,
		Session: sessionOf(r),
		Label:   "download " + label + " → " + loc.Name,
		Body:    body,
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
		jobID, qErr := s.queue.Enqueue(r.Context(), queue.NewJob{
			Kind:    queue.KindCleanup,
			Session: sessionOf(r),
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
