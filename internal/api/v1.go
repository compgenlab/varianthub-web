package api

// The /api/v1 handlers (docs/api.md "Chunk 5").
//
// Scope: what a bulk programmatic consumer needs — resolve a snapshot to its
// pinned source versions, submit variants, poll the job, stream the whole result
// set. Cohort Studio annotates a genotype store's site catalog through exactly
// this path.
//
// Deliberately NOT here: GET /jobs/{id}/results. Paginated, sorted, filtered rows
// need the results-storage question in docs/api.md settled first (a result is one
// opaque JSON blob today, which cannot serve a sorted page). That decision blocks
// the results handler but not export, and bulk consumers want the whole set
// anyway — so export ships now and results waits for the schema work.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/limit"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// maxVariantsPerRequest caps a JSON locus submission.
//
// The engine receives loci as argv (runner.ExecRunner splits Body on whitespace
// and appends each as an argument), so a large batch does not fail cleanly — it
// trips the kernel's ARG_MAX somewhere north of a couple of megabytes and the
// exec fails for a reason that looks nothing like "too many variants". Cap it
// well below that and point callers at the VCF path, which streams through a file
// and has no such ceiling.
const maxVariantsPerRequest = 10000

// normalizeLocus accepts the dash-delimited variant form docs/api.md uses in its
// examples and rewrites it to the colon-delimited form the engine parses.
//
// The engine splits a locus on ":" (varianthub-cli's ParseLocus), so submitting
// the documented "chr17-7676154-C-T" fails the job with `bad locus … want
// chrom:pos:ref:alt` — a confusing rejection of the exact shape the API
// advertises. Rather than change the documented shape or make every caller
// convert, translate here.
//
// The conversion is deliberately narrow: only a token with no colon, exactly four
// dash-separated non-empty fields, and a numeric second field is rewritten.
// Anything else passes through untouched, so colon-bearing HGVS
// ("NM_000546.6:c.215C>G") and rsIDs are never mangled.
func normalizeLocus(s string) string {
	if strings.Contains(s, ":") {
		return s
	}
	parts := strings.Split(s, "-")
	if len(parts) != 4 {
		return s
	}
	for _, p := range parts {
		if p == "" {
			return s
		}
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return s
	}
	return strings.Join(parts, ":")
}

// sessionOf identifies the submitter for history scoping.
func sessionOf(r *http.Request) string {
	if s := strings.TrimSpace(r.Header.Get("X-Varhub-Session")); s != "" {
		return s
	}
	if c, err := r.Cookie("varhub_session"); err == nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

// trustedCaller reports whether the caller may read *anyone's* jobs.
//
// Administrators only. Reading other people's results is an operator power, and
// an ordinary account has no business with it however it authenticated.
func (s *Server) trustedCaller(r *http.Request) bool {
	return callerOf(r).IsAdmin()
}

// throttled reports whether this caller is subject to the per-IP submit rate.
//
// Anonymous callers are; identified ones are not. The rate limit exists to stop
// an unaccountable browser flooding the queue, and an account is accountable —
// it can be disabled, and its jobs are attributable. Applying it to a signed-in
// bulk load would make that load throttle itself: a 3,000-site catalog submitted
// in chunks would stall on a 30-per-minute cap. The per-IP *concurrency* cap
// still applies to everyone, so one caller cannot monopolise the workers.
func (s *Server) throttled(r *http.Request) bool {
	return callerOf(r).Anonymous()
}

// --- catalog ---

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	snaps, err := s.catalog.ListSnapshots(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Drafts are selectable: a snapshot has to be usable before anyone can tell
	// whether it is worth publishing. Every entry carries its state so the client
	// can mark a draft as not-yet-fixed rather than the server hiding it.
	// ?state=published narrows to fixed ones for a caller that wants only those.
	if want := r.URL.Query().Get("state"); want != "" && want != "all" {
		kept := snaps[:0]
		for _, sn := range snaps {
			if sn.State == want {
				kept = append(kept, sn)
			}
		}
		snaps = kept
	}
	vis, err := s.visibilityFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type item struct {
		catalog.Snapshot
		SourceCount     int  `json:"source_count"`
		ContainsPrivate bool `json:"contains_private"`
		ContainsRemote  bool `json:"contains_remote"`
	}
	out := make([]item, 0, len(snaps))
	for _, sn := range snaps {
		// ListSnapshots does not populate Sources, and source_count /
		// contains_private both need them. Snapshots number in the single digits,
		// so a fetch each is cheaper than another query shape; revisit if that
		// stops being true.
		full, err := s.catalog.GetSnapshot(r.Context(), sn.ID)
		if err != nil {
			// The sources could not be read, so whether it is visible cannot be
			// decided. Omit it: listing a snapshot that might pin something
			// private is the one outcome with no way back.
			continue
		}
		if !vis.canSeeSnapshot(full) {
			continue
		}
		out = append(out, item{
			Snapshot:        sn,
			SourceCount:     len(full.Sources),
			ContainsPrivate: full.ContainsPrivate(),
			ContainsRemote:  full.ContainsRemote(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

// handleSnapshot resolves one snapshot to its pinned source versions.
//
// This is the reproducibility hook: a consumer that records "annotated under
// snapshot X" needs to know exactly which ClinVar and gnomAD releases that meant,
// so a count can be reproduced or a refresh diffed against it.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	snap, err := s.catalog.GetSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such snapshot")
		return
	}
	vis, err := s.visibilityFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !vis.canSeeSnapshot(snap) {
		// 404, not 403. A snapshot's name and the fact that it exists are
		// themselves information about what this installation holds, and a 403
		// would confirm both to someone who guessed.
		writeError(w, http.StatusNotFound, "no such snapshot")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot":         snap,
		"contains_private": snap.ContainsPrivate(),
		"contains_remote":  snap.ContainsRemote(),
		"annotations":      snapshotAnnotations(snap),
	})
}

// snapshotAnnotations lists every field the snapshot's sources can contribute,
// each attributed to its source and flagged if the snapshot applies it by
// default. This is what the annotation flow's field picker renders.
func snapshotAnnotations(snap catalog.Snapshot) []annotationOption {
	def := map[string]bool{}
	for _, d := range snap.Defaults {
		def[d] = true
	}
	out := []annotationOption{}
	seen := map[string]bool{}
	for _, src := range snap.Sources {
		for _, a := range src.Annotations() {
			if seen[a.Name] {
				continue // two sources naming the same field: first wins, as varhub does
			}
			seen[a.Name] = true
			out = append(out, annotationOption{Annotation: a, Default: def[a.Name]})
		}
	}
	return out
}

type annotationOption struct {
	catalog.Annotation
	Default bool `json:"default"`
}

// handleSources lists sources one row per (name, version).
//
// docs/api.md sketches a shape that groups versions under a source id. This
// returns them flat instead, because a consumer pinning annotations needs to
// address an exact version, and grouping loses which version a given snapshot
// pinned. The grouped view is a presentation concern the SPA can derive.
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	srcs, err := s.catalog.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	vis, err := s.visibilityFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	srcs = vis.filterSources(srcs)

	type item struct {
		catalog.Source
		Ref string `json:"ref"` // "name:version", how a snapshot manifest pins it
		// Annotations lets the flow show which fields a source contributes before
		// it is chosen, so picking sources and picking fields are one step.
		Annotations []catalog.Annotation `json:"annotations"`
		// NeedsData is false for builtins, which compute from the variant and have
		// nothing to download.
		NeedsData bool `json:"needs_data"`
	}
	out := make([]item, 0, len(srcs))
	for _, src := range srcs {
		anns := src.Annotations()
		if anns == nil {
			anns = []catalog.Annotation{}
		}
		out = append(out, item{
			Source: src, Ref: src.Ref(), Annotations: anns, NeedsData: src.NeedsData(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// --- annotation ---

// annotateRequest is the JSON body of POST /api/v1/annotate.
type annotateRequest struct {
	Snapshot    string   `json:"snapshot"`
	Sources     []string `json:"sources"` // individual-source selection (needs Build)
	Build       string   `json:"build"`   // assembly, required with Sources
	Variants    []string `json:"variants"`
	Annotations any      `json:"annotations"` // omitted | "all" | "a,b" | ["a","b"]
}

// resolveSnapshot turns a request's snapshot-or-sources into a snapshot name.
//
// An individual-source selection still becomes a snapshot: that is what the
// engine annotates against, what gets materialized, and what makes the job
// reproducible afterwards. The row is deterministic in the selection, so
// resubmitting the same set reuses it instead of accumulating one per job.
func (s *Server) resolveSnapshot(r *http.Request, snapshot string, sources []string,
	build string, defaults []string) (string, error) {

	if snapshot != "" && len(sources) > 0 {
		return "", errors.New(`give either "snapshot" or "sources", not both`)
	}
	if snapshot != "" {
		if err := s.checkSnapshotVisible(r, snapshot); err != nil {
			return "", err
		}
		return snapshot, nil
	}
	if len(sources) == 0 {
		return "", errors.New(`"snapshot" or "sources" is required`)
	}
	if s.catalog == nil {
		return "", errors.New("catalog unavailable; annotate with a snapshot instead")
	}
	if strings.TrimSpace(build) == "" {
		return "", errors.New(`"build" is required when selecting individual sources`)
	}
	if err := s.checkSourcesVisible(r, sources); err != nil {
		return "", err
	}
	return s.catalog.EnsureAdhocSnapshot(r.Context(), build, sources, defaults)
}

// checkSnapshotVisible refuses to annotate against a snapshot the caller cannot
// see. Without this, a snapshot hidden from the listing is still annotatable by
// name — and the results would carry exactly the private annotations the hiding
// was meant to withhold.
func (s *Server) checkSnapshotVisible(r *http.Request, id string) error {
	if s.catalog == nil {
		return nil // no catalog to check against; the runner will fail loudly
	}
	snap, err := s.catalog.GetSnapshot(r.Context(), id)
	if err != nil {
		return nil // unknown snapshot: let the existing not-found path report it
	}
	vis, err := s.visibilityFor(r)
	if err != nil {
		return err
	}
	if !vis.canSeeSnapshot(snap) {
		return fmt.Errorf("no such snapshot: %q", id)
	}
	return nil
}

// checkSourcesVisible refuses a selection containing a source the caller cannot
// see, naming the ones at fault so a mistaken ref is distinguishable from a
// missing grant.
func (s *Server) checkSourcesVisible(r *http.Request, ids []string) error {
	vis, err := s.visibilityFor(r)
	if err != nil {
		return err
	}
	if vis.admin {
		return nil
	}
	var srcs []catalog.Source
	for _, id := range ids {
		src, err := s.catalog.GetSource(r.Context(), id)
		if err != nil {
			continue // unknown source: PutSnapshot reports it with better context
		}
		srcs = append(srcs, src)
	}
	if hidden := vis.hiddenSources(srcs); len(hidden) > 0 {
		return fmt.Errorf("no such source: %s", strings.Join(hidden, ", "))
	}
	return nil
}

// selection normalizes the `annotations` field to the runner's Selection string:
// "" for the snapshot's defaults, "all", or a comma-joined list.
func selection(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(x), nil
	case []any:
		names := make([]string, 0, len(x))
		for _, it := range x {
			s, ok := it.(string)
			if !ok {
				return "", fmt.Errorf("annotations must be strings")
			}
			if s = strings.TrimSpace(s); s != "" {
				names = append(names, s)
			}
		}
		return strings.Join(names, ","), nil
	default:
		return "", fmt.Errorf("annotations must be a string or an array of strings")
	}
}

// splitSelection turns a stored selection back into names, for seeding an
// ad-hoc snapshot's defaults. "all" and "" carry no explicit list.
func splitSelection(sel string) []string {
	if sel == "" || sel == "all" {
		return nil
	}
	return strings.Split(sel, ",")
}

func (s *Server) handleAnnotate(w http.ResponseWriter, r *http.Request) {
	var in annotateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<24)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	loci := make([]string, 0, len(in.Variants))
	for _, v := range in.Variants {
		if v = strings.TrimSpace(v); v != "" {
			loci = append(loci, normalizeLocus(v))
		}
	}
	if len(loci) == 0 {
		writeError(w, http.StatusBadRequest, "`variants` is required and must be non-empty")
		return
	}
	if len(loci) > maxVariantsPerRequest {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"%d variants exceeds the %d-variant limit for this endpoint; use POST /api/v1/annotate/vcf for bulk submissions",
			len(loci), maxVariantsPerRequest))
		return
	}
	sel, err := selection(in.Annotations)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// An individual-source selection becomes a snapshot here, so everything
	// downstream — materialization, results columns, reproducibility — is
	// identical whichever way the caller chose.
	snapshot, err := s.resolveSnapshot(r, strings.TrimSpace(in.Snapshot), in.Sources,
		in.Build, splitSelection(sel))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	label := loci[0]
	if len(loci) > 1 {
		label = fmt.Sprintf("%s +%d more", loci[0], len(loci)-1)
	}
	s.submit(w, r, queue.NewJob{
		Kind:      queue.KindLocus,
		Snapshot:  snapshot,
		Selection: sel,
		Session:   sessionOf(r),
		UserID:    callerOf(r).UserID(),
		Label:     label,
		Body:      []byte(strings.Join(loci, "\n")),
	})
}

func (s *Server) handleAnnotateVCF(w http.ResponseWriter, r *http.Request) {
	max := s.cfg.MaxUploadBytes
	if max <= 0 {
		max = 64 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	// ParseMultipartForm's argument is the in-memory threshold, not the total
	// cap; MaxBytesReader above is what actually bounds the upload.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"could not read upload (limit %d bytes): %v", max, err))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, hdr, err := r.FormFile("vcf")
	if err != nil {
		writeError(w, http.StatusBadRequest, "a `vcf` file part is required")
		return
	}
	defer file.Close()

	// MaxBytesReader above already bounds this; ParseMultipartForm has spilled
	// anything over the in-memory threshold to a temp file that RemoveAll clears.
	body, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the uploaded VCF")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "the uploaded VCF is empty")
		return
	}

	sel, err := selection(anyOf(r.FormValue("annotations")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var sources []string
	if v := strings.TrimSpace(r.FormValue("sources")); v != "" {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				sources = append(sources, id)
			}
		}
	}
	snapshot, err := s.resolveSnapshot(r, strings.TrimSpace(r.FormValue("snapshot")),
		sources, strings.TrimSpace(r.FormValue("build")), splitSelection(sel))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.submit(w, r, queue.NewJob{
		Kind:      queue.KindVCF,
		Snapshot:  snapshot,
		Selection: sel,
		Session:   sessionOf(r),
		UserID:    callerOf(r).UserID(),
		Label:     hdr.Filename,
		Body:      body,
	})
}

// anyOf lifts an empty form value to nil so selection() reads it as "unset"
// rather than as an empty selection.
func anyOf(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// submit enqueues a job and, when ?wait= is given, blocks briefly so a fast job
// can return its result inline instead of forcing the caller to poll.
func (s *Server) submit(w http.ResponseWriter, r *http.Request, nj queue.NewJob) {
	nj.ClientIP = limit.ClientIP(r, s.trusted)
	id, err := s.queue.Enqueue(r.Context(), nj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	wait := s.waitFor(r)
	if wait <= 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id})
		return
	}
	job, ok, err := s.queue.WaitFor(r.Context(), id, wait)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok || !job.Terminal() {
		// Still running when the window closed: 202 and the caller polls. This is
		// not an error -- ?wait= is an optimization, not a guarantee.
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id, "status": job.Status})
		return
	}
	s.writeJobWithResult(w, r, job)
}

// waitFor parses ?wait= (seconds or a Go duration), clamped to the server cap.
func (s *Server) waitFor(r *http.Request) time.Duration {
	raw := strings.TrimSpace(r.URL.Query().Get("wait"))
	if raw == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(raw); err == nil {
		d = time.Duration(n) * time.Second
	} else if parsed, err := time.ParseDuration(raw); err == nil {
		d = parsed
	} else {
		return 0
	}
	if d < 0 {
		return 0
	}
	if cap := s.cfg.SubmitWaitCap; cap > 0 && d > cap {
		d = cap
	}
	return d
}

// --- jobs ---

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := clampInt(q.Get("limit"), 50, 1, 500)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	f := queue.JobFilter{Status: strings.TrimSpace(q.Get("status"))}

	// Annotation jobs only, by default. A download is operational work — it has no
	// variants and no results table — so listing it alongside someone's
	// annotations is noise in the view they actually came for. ?kind=download (or
	// =all) is how the admin job log asks for the rest.
	switch strings.TrimSpace(q.Get("kind")) {
	case "", "annotation":
		f.Kinds = []string{queue.KindLocus, queue.KindVCF}
	case "all":
		// no kind constraint
	case "download", "system":
		// "download" kept as an alias; the admin log wants every operational job.
		f.Kinds = []string{queue.KindDownload, queue.KindCleanup}
	default:
		writeError(w, http.StatusBadRequest,
			"invalid kind filter (want annotation, download, system or all)")
		return
	}

	scoped := !s.trustedCaller(r)
	if scoped {
		// An untrusted caller sees only their own submissions. An account scopes
		// by user id, which the server wrote from a verified credential; an
		// anonymous visitor falls back to the session id they assert, which
		// scopes their own browser history and nothing more.
		//
		// With neither there is nothing to scope to, so return nothing rather
		// than everything — the failure mode of a leak is worse than an empty
		// list.
		if uid := callerOf(r).UserID(); uid != "" {
			f.UserID = uid
		} else if sess := sessionOf(r); sess != "" {
			f.Session = sess
		} else {
			writeJSON(w, http.StatusOK, map[string]any{
				"jobs": []any{}, "limit": limit, "offset": offset, "scoped": true,
			})
			return
		}
	}
	jobs, err := s.queue.List(r.Context(), f, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []queue.Job{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": jobs, "limit": limit, "offset": offset, "scoped": scoped,
	})
}

// job loads a job and enforces ownership, writing the error response itself.
//
// Knowing a job id is not authorization: ids are handed out to whoever submitted
// and could be logged or shared. An untrusted caller must own the job — by
// account where it has one, and only otherwise by the session that created it.
func (s *Server) job(w http.ResponseWriter, r *http.Request) (queue.Job, bool) {
	id := r.PathValue("id")
	job, ok, err := s.queue.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return queue.Job{}, false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return queue.Job{}, false
	}
	if !s.trustedCaller(r) && !s.owns(r, job) {
		// 404 rather than 403: confirming a job exists is itself a small leak.
		writeError(w, http.StatusNotFound, "no such job")
		return queue.Job{}, false
	}
	return job, true
}

// owns reports whether the caller submitted this job.
//
// A job with an owning account is readable only by that account: the session id
// on it is client-asserted, so honouring it as well would let anyone who learned
// the string read a signed-in user's results.
func (s *Server) owns(r *http.Request, job queue.Job) bool {
	if job.UserID != "" {
		return callerOf(r).UserID() == job.UserID
	}
	sess := sessionOf(r)
	return sess != "" && sess == job.Session
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.job(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleExport streams a finished job's entire result set.
//
// This is the bulk path: no pagination, no sorting, no filtering. A consumer
// annotating a whole site catalog wants every row exactly as the engine emitted
// it, and the stored blob is already that -- so it is copied through verbatim
// rather than decoded and re-encoded.
// writeJobWithResult returns the job object with its results embedded, which is
// what ?wait= promises on completion within the window.
func (s *Server) writeJobWithResult(w http.ResponseWriter, r *http.Request, job queue.Job) {
	out := map[string]any{
		"job_id": job.ID, "kind": job.Kind, "snapshot": job.Snapshot,
		"status": job.Status, "n_variants": job.NVariants,
		"created_at": job.CreatedAt, "started_at": job.StartedAt,
		"finished_at": job.FinishedAt, "label": job.Label,
	}
	if job.Status == queue.StatusError {
		out["error"] = job.Error
		writeJSON(w, http.StatusOK, out)
		return
	}
	body, ok, err := s.queue.Result(r.Context(), job.ID)
	if err == nil && ok && len(body) > 0 {
		out["results"] = json.RawMessage(body)
	}
	writeJSON(w, http.StatusOK, out)
}

func clampInt(raw string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
