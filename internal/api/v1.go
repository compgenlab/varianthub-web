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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/limit"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

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
//
// The server-issued anonymous session, not the X-Varhub-Session header this
// used to read. That header was a random id the browser generated for itself,
// so it scoped a history but proved nothing — and being indistinguishable from
// a value anyone could send is what let a bare curl look like a visitor.
//
// Empty for a signed-in caller, whose jobs scope by account instead.
func sessionOf(r *http.Request) string {
	return callerOf(r).Scope()
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

	out := make([]SnapshotSummary, 0, len(snaps))
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
		out = append(out, SnapshotSummary{
			Snapshot:        sn,
			SourceCount:     len(full.Sources),
			Visibility:      full.EffectiveVisibility(),
			ConstrainedBy:   snapshotConstraints(full),
			ContainsPrivate: full.ContainsPrivate(),
			ContainsRemote:  full.ContainsRemote(),
		})
	}
	writeJSON(w, http.StatusOK, SnapshotsResponse{Snapshots: out})
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
	writeJSON(w, http.StatusOK, SnapshotResponse{
		Snapshot:        snap,
		Visibility:      snap.EffectiveVisibility(),
		ConstrainedBy:   snapshotConstraints(snap),
		ContainsPrivate: snap.ContainsPrivate(),
		ContainsRemote:  snap.ContainsRemote(),
		Annotations:     snapshotAnnotations(snap),
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
	Default bool `json:"default" doc:"Selected when the caller asks for no annotations by name."`
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

	states, err := s.catalog.SourceStates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// What is in flight comes from the queue; what was installed comes from the
	// catalog, because terminal jobs are collected and the record has to outlive
	// them.
	active := map[string]string{}
	if s.queue != nil {
		if a, aErr := s.queue.ActiveDownloads(r.Context()); aErr == nil {
			active = a
		}
	}

	out := make([]SourceItem, 0, len(srcs))
	for _, src := range srcs {
		anns := src.Annotations()
		if anns == nil {
			anns = []catalog.Annotation{}
		}
		st := states[src.ID]
		if job, ok := active[src.ID]; ok {
			st.State, st.Job = catalog.StateInstalling, job
		}
		if st.State == "" && !src.NeedsData() {
			// A builtin or a streamed source has nothing to provision, so it is
			// usable the moment it is registered.
			st.State = catalog.StateReady
		}
		out = append(out, SourceItem{
			Source: src, Ref: src.Ref(), Annotations: anns,
			NeedsData: src.NeedsData(), State: st,
			RequiresReference: src.RequiresReference(), IsReference: src.IsReference(),
			GeneListGTF: src.GeneListGTF(),
		})
	}
	writeJSON(w, http.StatusOK, SourcesResponse{Sources: out})
}

// --- annotation ---

// annotateRequest is the JSON body of POST /api/v1/annotate.
type annotateRequest struct {
	Snapshot    string   `json:"snapshot"`
	Sources     []string `json:"sources"` // individual-source selection (needs Build)
	Build       string   `json:"build"`   // assembly, required with Sources
	Variants    []string `json:"variants"`
	Annotations any      `json:"annotations"` // omitted | "all" | "a,b" | ["a","b"]
	CallbackURL string   `json:"callback_url"`
}

// callbackURL validates a requested callback and says who may have one.
//
// Named callers only. An anonymous visitor is a session that outlives nothing,
// and handing one the ability to make this service issue an HTTP request to an
// address of their choosing is the SSRF surface with the accountability removed
// — the address checks still apply, but nobody is left to ask about it
// afterwards. Anonymous callers poll, which is what a browser does anyway.
//
// The URL is checked here for shape and again, properly, when it is dialled:
// this catches a typo while somebody is watching, and cannot say where a name
// will point hours later when the job finishes. See internal/callback.
func (s *Server) callbackURL(r *http.Request, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if callerOf(r).Anonymous() {
		return "", errors.New(
			"a callback needs an account; anonymous submissions are polled for")
	}
	if err := s.callbacks.ValidateURL(raw); err != nil {
		return "", err
	}
	return raw, nil
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
	// The caller's own cap, not a fixed number. Checked here because a locus
	// list's variant count is the length of a slice — the VCF path cannot do
	// this at the door, since counting a compressed file means decompressing it.
	_, lim := s.callerLimits(r, limit.ClientIP(r, s.trusted))
	if !lim.AllowsVariants(len(loci)) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"%d variants exceeds the %d-variant limit for this account; use POST /api/v1/annotate/vcf for bulk submissions",
			len(loci), lim.MaxVariants))
		return
	}
	sel, err := selection(in.Annotations)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cb, err := s.callbackURL(r, in.CallbackURL)
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

	// The list becomes a VCF here, so a job's stored input is always one
	// whatever it was submitted as. Everything downstream — the worker, the
	// cache, the engine — then has a file rather than two shapes to tell apart.
	//
	// The id is minted before the object because the object is stored under it,
	// which is what makes a bucket listing say which job every object belongs
	// to. Same order as the upload path, for the same reason.
	id, err := queue.NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	uri := queue.ObjectURI(s.cfg.JobStorage, id, queue.InputName(true))
	pr, pw := io.Pipe()
	go func() {
		_, wErr := writeLocusVCF(pw, loci)
		pw.CloseWithError(wErr)
	}()
	if err := blob.PutReader(r.Context(), uri, pr); err != nil {
		pr.CloseWithError(err)
		log.Printf("api: store loci for job %s at %s: %v", id, uri, err)
		writeError(w, http.StatusInternalServerError, "could not store the submitted variants")
		return
	}

	if !s.submit(w, r, queue.NewJob{
		ID:          id,
		Kind:        queue.KindLocus,
		Snapshot:    snapshot,
		Selection:   sel,
		Session:     sessionOf(r),
		UserID:      callerOf(r).UserID(),
		Label:       label,
		InputURI:    uri,
		CallbackURL: cb,
		// Kept alongside the stored file until the worker reads the file
		// instead. Two copies of a locus list is a few hundred bytes; two code
		// paths, briefly, is the thing being removed.
		Body: []byte(strings.Join(loci, "\n")),
	}) {
		// The row was refused, so nothing will ever read the object. Removed
		// here because this is the last place that knows where it went.
		if rmErr := blob.Remove(context.WithoutCancel(r.Context()), uri); rmErr != nil {
			log.Printf("api: remove orphaned loci object %s: %v", uri, rmErr)
		}
	}
}

// maxFormField bounds one non-file part of an upload. Snapshot names and
// annotation lists are tens of bytes; anything approaching this is not one.
const maxFormField = 1 << 20

// maxFormFields bounds how many non-file parts one request may carry, so a
// client cannot make the server accumulate an unbounded number of small
// strings while claiming to be uploading a VCF.
const maxFormFields = 32

func (s *Server) handleAnnotateVCF(w http.ResponseWriter, r *http.Request) {
	max := s.cfg.MaxUploadBytes
	if max <= 0 {
		max = 64 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)

	// Read the parts as they arrive rather than through ParseMultipartForm.
	//
	// That one buffers the whole request before any of it can be looked at — a
	// few megabytes in memory and the rest spilled to a temp file — so the API
	// needed local scratch proportional to the upload cap, and the file was
	// written to disk once on the way to being written to storage.
	//
	// Streaming removes both. The trade is that a part is available only when
	// it arrives, so a request whose file comes before its metadata is stored
	// before it can be validated. That is a smaller cost than it looks:
	// buffering did not avoid *receiving* the bytes either, it only delayed
	// them, and what is wasted here is the hop to job storage, which is
	// internal and undone by a delete.
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart form upload")
		return
	}

	var (
		fields   = map[string]string{}
		filename string
		id       string
		uri      string
	)
	// Whatever was stored before an error, so every failure path below can undo
	// it. Without this an upload that arrives before a metadata field it turns
	// out to conflict with would be left in storage with nothing pointing at it.
	discard := func() {
		if uri == "" {
			return
		}
		if rmErr := blob.Remove(context.WithoutCancel(r.Context()), uri); rmErr != nil {
			log.Printf("api: upload at %s was not turned into a job and could not be "+
				"removed: %v", uri, rmErr)
		}
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			discard()
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"could not read the upload (limit %d bytes): %v", max, err))
			return
		}

		if part.FormName() != "vcf" {
			if len(fields) >= maxFormFields {
				part.Close()
				discard()
				writeError(w, http.StatusBadRequest, "too many form fields")
				return
			}
			v, err := io.ReadAll(io.LimitReader(part, maxFormField))
			part.Close()
			if err != nil {
				discard()
				writeError(w, http.StatusBadRequest, "could not read a form field")
				return
			}
			fields[part.FormName()] = string(v)
			continue
		}

		if uri != "" {
			part.Close()
			discard()
			writeError(w, http.StatusBadRequest, "only one `vcf` part is allowed")
			return
		}
		filename = part.FileName()

		// Classify the compression once, here, and record it in the object's
		// name. This is the only place that looks at the bytes to decide; every
		// later reader is told by the filename instead of working it out again.
		// The process that received the file is the one that knows, and four
		// consumers each sniffing for themselves is four chances to disagree
		// about one file.
		//
		// Buffered rather than seeked, because a streamed part cannot rewind —
		// the two bytes are read and then put back in front of the stream.
		buf := bufio.NewReader(part)
		magic, err := buf.Peek(2)
		if len(magic) == 0 {
			part.Close()
			discard()
			writeError(w, http.StatusBadRequest, "the uploaded VCF is empty")
			return
		}
		if err != nil && !errors.Is(err, io.EOF) {
			part.Close()
			discard()
			writeError(w, http.StatusBadRequest, "could not read the uploaded VCF")
			return
		}
		compressed := len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b

		// The id is minted before the object because the object is stored under
		// it, which is what makes a bucket listing say which job every object
		// belongs to.
		if id, err = queue.NewID(); err != nil {
			part.Close()
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		dest := queue.ObjectURI(s.cfg.JobStorage, id, queue.InputName(compressed))
		if err := blob.PutReader(r.Context(), dest, buf); err != nil {
			part.Close()
			// dest is not assigned to uri until it exists, so discard() has
			// nothing to undo — PutReader leaves no partial object behind.
			log.Printf("api: store upload for job %s at %s: %v", id, dest, err)
			writeError(w, http.StatusInternalServerError, "could not store the uploaded VCF")
			return
		}
		part.Close()
		uri = dest
	}

	if uri == "" {
		writeError(w, http.StatusBadRequest, "a `vcf` file part is required")
		return
	}

	sel, err := selection(anyOf(strings.TrimSpace(fields["annotations"])))
	if err != nil {
		discard()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var sources []string
	if v := strings.TrimSpace(fields["sources"]); v != "" {
		for _, sid := range strings.Split(v, ",") {
			if sid = strings.TrimSpace(sid); sid != "" {
				sources = append(sources, sid)
			}
		}
	}
	cb, err := s.callbackURL(r, fields["callback_url"])
	if err != nil {
		discard()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	snapshot, err := s.resolveSnapshot(r, strings.TrimSpace(fields["snapshot"]),
		sources, strings.TrimSpace(fields["build"]), splitSelection(sel))
	if err != nil {
		discard()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Every uploaded VCF goes through the split, whatever its size. A small one
	// produces a single chunk, which is a job of one chunk and travels the same
	// path as a job of two hundred.
	//
	// There is deliberately no threshold. A threshold means two ways a
	// submission can be processed, only one of which is exercised by ordinary
	// use — so the other is the one that breaks, and it breaks for the largest
	// files, which are the ones nobody wants to resubmit.
	if !s.submit(w, r, queue.NewJob{
		ID:          id,
		Kind:        queue.KindSplit,
		Snapshot:    snapshot,
		Selection:   sel,
		Session:     sessionOf(r),
		UserID:      callerOf(r).UserID(),
		Label:       filename,
		InputURI:    uri,
		CallbackURL: cb,
	}) {
		discard()
	}
}

// anyOf lifts an empty form value to nil so selection() reads it as "unset"
// rather than as an empty selection.
func anyOf(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// submit enqueues a job and answers with its identifier.
//
// Always 202, never a result. Submission used to take ?wait= and block briefly
// so a fast job could come back inline, which made one call return two
// different shapes depending on how quickly the work happened to finish. Every
// client had to handle both regardless, because the window was never a
// guarantee — so the shape that was always required is now the only one, and
// each of status, results and cancellation is its own call.
// submit records the job and writes the response, reporting whether it was
// accepted.
//
// The boolean exists for the upload path: when the row cannot be written, the
// object already in storage has to be removed, and the caller is the only one
// that still knows where it is.
func (s *Server) submit(w http.ResponseWriter, r *http.Request, nj queue.NewJob) bool {
	nj.ClientIP = limit.ClientIP(r, s.trusted)

	// Stamped at submit, not read at dispatch. The limit a job runs under is the
	// one that applied when it was accepted, so raising somebody's tier does not
	// retroactively change what is already queued — and the dispatcher stays a
	// single statement over one table rather than joining the identity schema.
	c := callerOf(r)
	_, lim := s.callerLimits(r, nj.ClientIP)
	nj.MaxConcurrent = lim.Concurrent
	nj.MaxVariants = lim.MaxVariants
	nj.Origin = queue.OriginWeb
	if c.ViaToken {
		nj.Origin = queue.OriginAPI
	}
	id, err := s.queue.Submit(r.Context(), nj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	writeJSON(w, http.StatusAccepted, AcceptedResponse{JobID: id})
	return true
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
			writeJSON(w, http.StatusOK, JobsResponse{
				Jobs: []JobStatusResponse{}, Limit: limit, Offset: offset, Scoped: true,
			})
			return
		}
	}
	jobs, err := s.queue.ListJobs(r.Context(), f, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]JobStatusResponse, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobStatus(j))
	}
	writeJSON(w, http.StatusOK, JobsResponse{
		Jobs: out, Limit: limit, Offset: offset, Scoped: scoped,
	})
}

// lookupJob loads a job by id, writing the error response itself. It enforces
// nothing — the caller decides which permission applies.
func (s *Server) lookupJob(w http.ResponseWriter, r *http.Request) (queue.Job, bool) {
	// Guarded like the catalog handlers are. This was unreachable while every
	// job route required a credential first: the 401 came before anything
	// touched the queue. Reading a shared link needs no credential, so an
	// installation without a queue would have answered with a panic.
	if s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return queue.Job{}, false
	}
	id := r.PathValue("id")
	job, ok, err := s.queue.GetJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return queue.Job{}, false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return queue.Job{}, false
	}
	return job, true
}

// job loads a job the caller may read, writing the error response itself.
//
// Reading, not changing: an anonymous job's id is enough to read it, and not
// enough to cancel it. See canView and owns.
func (s *Server) job(w http.ResponseWriter, r *http.Request) (queue.Job, bool) {
	job, ok := s.lookupJob(w, r)
	if !ok {
		return queue.Job{}, false
	}
	if !s.trustedCaller(r) && !s.canView(r, job) {
		// 404 rather than 403: confirming a job exists is itself a small leak.
		writeError(w, http.StatusNotFound, "no such job")
		return queue.Job{}, false
	}
	return job, true
}

// canView reports whether the caller may read this job and its results.
//
// A job with an account is private to that account. A job without one is
// readable by anyone holding its id: there is no account to attach it to, the
// id is 128 unguessable bits, and so the link is the credential. That is what
// makes an anonymous result shareable — it can be sent to a colleague or
// reopened on another machine, for work that was anonymous to begin with and
// has no account to protect.
func (s *Server) canView(r *http.Request, job queue.Job) bool {
	if job.UserID == "" {
		return true
	}
	return s.owns(r, job)
}

// owns reports whether the caller submitted this job. It is the stricter of the
// two checks, and gates changing a job rather than reading one.
//
// The split matters for anonymous jobs: the link is enough to read a result,
// and deliberately not enough to cancel the run behind it. Forwarding a link so
// someone can look at the output should not also hand them the ability to stop
// it — reading is why the link was shared, and stopping is not.
//
// For a job with an account, the account is the whole answer: the session id on
// it is client-asserted, so honouring that as well would let anyone who learned
// the string act as a signed-in user.
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
	// The chunks come back with it. See JobResponse: which piece of a split
	// submission failed is part of the job's status, not a separate thing to go
	// and ask about.
	chunks, err := s.queue.JobChunks(r.Context(), job.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobDetail(job, chunks))
}

// handleCancelJob stops a job.
//
// Gated by the same ownership rule as reading it: a caller who may see a job may
// stop it. Cancelling is strictly less powerful than submitting — someone who
// can start work can already occupy a worker, and letting them stop it again
// frees the slot they are holding rather than taking anything from anyone else.
// An administrator can cancel anything, which is what the system jobs view uses.
// ownedJob is job, for the things only the submitter may do.
//
// Separate from job because the two genuinely differ for anonymous work: its
// link is readable by anyone holding it, and cancellation is not.
func (s *Server) ownedJob(w http.ResponseWriter, r *http.Request) (queue.Job, bool) {
	job, ok := s.lookupJob(w, r)
	if !ok {
		return queue.Job{}, false
	}
	if !s.trustedCaller(r) && !s.owns(r, job) {
		writeError(w, http.StatusNotFound, "no such job")
		return queue.Job{}, false
	}
	return job, true
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	// ownedJob, not job: a shared link reads a result, it does not stop the run.
	job, ok := s.ownedJob(w, r)
	if !ok {
		return
	}
	out, err := s.queue.CancelJob(r.Context(), job.ID)
	switch {
	case errors.Is(err, queue.ErrNotCancellable):
		// Not an error worth a failure status: the caller wanted it stopped and
		// it is stopped. Report the state so the UI can settle on it.
		writeJSON(w, http.StatusOK, CancelResponse{
			Job: jobStatus(out), Cancelled: false,
			Detail: "job had already finished",
		})
		return
	case errors.Is(err, queue.ErrNoSuchJob):
		writeError(w, http.StatusNotFound, "no such job")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("api: job %s cancelled by %s", job.ID, callerOf(r).Label())
	writeJSON(w, http.StatusOK, CancelResponse{Job: jobStatus(out), Cancelled: true})
}

// handleJobLog serves what a job's run printed.
//
// Separate from the job itself because it is large and wanted only when someone
// is looking into a particular run — the same reason the results blob is its own
// endpoint. Ownership is enforced by the same rule: a log describes a job, so
// seeing it is seeing the job.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	job, ok := s.job(w, r)
	if !ok {
		return
	}
	out, found, err := s.queue.Log(r.Context(), job.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id": job.ID,
		"output": out,
		// Distinguishes "nothing was recorded" from "it printed nothing", which
		// look identical in an empty string and mean different things: the first
		// is a job from before logs were kept, the second is a quiet run.
		"recorded": found,
	})
}

// --- chunks ---

// jobChunk loads one chunk of a job the caller may read.
//
// Underneath the job, never beside it: a chunk id means nothing on its own, and
// a route that took one directly would need its own entitlement check over a
// row that carries no owner. Reaching it through the job means the job's rule
// is the only rule.
func (s *Server) jobChunk(w http.ResponseWriter, r *http.Request) (queue.Chunk, bool) {
	job, ok := s.job(w, r)
	if !ok {
		return queue.Chunk{}, false
	}
	c, found, err := s.queue.JobChunk(r.Context(), job.ID, r.PathValue("chunkId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return queue.Chunk{}, false
	}
	if !found {
		// 404 for a chunk of another job as much as for one that never existed:
		// the lookup is scoped to this job, so "not yours" and "not there" are
		// the same answer and neither confirms anything.
		writeError(w, http.StatusNotFound, "no such chunk")
		return queue.Chunk{}, false
	}
	return c, true
}

// handleChunkLog serves what one chunk printed.
//
// The only thing a chunk has that GET /jobs/{id} does not already return, which
// is why it is the only route under a chunk id. The job's own log is its first
// chunk's — the run a caller submitted, or the split that cut it up — and this
// is how the other twenty-five are read, one at a time, which a job log that
// concatenated them would make impossible to avoid.
func (s *Server) handleChunkLog(w http.ResponseWriter, r *http.Request) {
	c, ok := s.jobChunk(w, r)
	if !ok {
		return
	}
	out, found, err := s.queue.ChunkLog(r.Context(), c.JobID, c.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":   c.JobID,
		"chunk_id": c.ID,
		"output":   out,
		"recorded": found,
	})
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
