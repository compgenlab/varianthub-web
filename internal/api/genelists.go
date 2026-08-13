package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

// The gene-list builder.
//
// A gene list is an ordinary catalog source — type = "genelist" — and everything
// downstream of registration already handles it: the picker, snapshots, grants,
// the annotation cache, the rule that pins its GTF alongside it. What was missing
// was a way to make one without writing TOML by hand and getting the gene names
// exactly right, which is the part a person cannot do reliably and a database can.
//
// So these two endpoints are the whole feature: check a pasted list against a
// GTF's genes, and turn a checked list into a manifest.

// geneListRequest is a pasted list plus the gene model to resolve it through.
type geneListRequest struct {
	GTFSourceID string `json:"gtf_source_id"`
	// Genes as pasted — newlines, commas, tabs, whatever the user had. Parsed
	// rather than required in a particular shape; see catalog.ParseGeneList.
	Genes string `json:"genes"`
	// GeneField is "gene_name" (default) or "gene_id".
	GeneField string `json:"gene_field,omitempty"`

	// The rest is only read when creating.
	Name           string `json:"name,omitempty"`
	Version        string `json:"version,omitempty"`
	Title          string `json:"title,omitempty"`
	Description    string `json:"description,omitempty"`
	AnnotationName string `json:"annotation_name,omitempty"`
	Visibility     string `json:"visibility,omitempty"`
}

func (req geneListRequest) byID() bool {
	return strings.EqualFold(strings.TrimSpace(req.GeneField), "gene_id")
}

// handleValidateGeneList reports which of the pasted genes the GTF does not have.
//
// Its own endpoint rather than a step inside creation because the two are asked
// at different times: the user validates while editing, repeatedly, and creates
// once. Creation validates again anyway — a form that checked at one moment and
// saved at another is a form that can save an unchecked list.
func (s *Server) handleValidateGeneList(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req geneListRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	res, err := s.checkGeneList(r, req)
	if err != nil {
		writeGeneListError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// geneListCheck is what a validation says about a pasted list.
type geneListCheck struct {
	GTF       string   `json:"gtf" doc:"The gene model the list was checked against, as \"name:version\"."`
	GeneField string   `json:"gene_field" doc:"gene_name | gene_id — which GTF attribute the list is matched on."`
	Total     int      `json:"total" doc:"How many distinct genes were found in the pasted text."`
	Known     int      `json:"known" doc:"How many of them the gene model has."`
	Unknown   []string `json:"unknown,omitempty" doc:"The ones it does not, spelled as they were typed. A list cannot be saved while this is non-empty."`
	Genes     []string `json:"genes,omitempty" doc:"The parsed list, upper-cased and deduplicated, in the order given. Shown back so the user can see what was actually read out of their paste."`
	Available int      `json:"available" doc:"How many genes the gene model has in total. Zero means it has not been scanned yet, which is why everything looks unknown."`
}

// checkGeneList parses and validates, and is the single place both endpoints get
// their answer from — so "validated" and "saved" cannot mean different things.
func (s *Server) checkGeneList(r *http.Request, req geneListRequest) (geneListCheck, error) {
	ctx := r.Context()

	gtf, err := s.catalog.GetSource(ctx, strings.TrimSpace(req.GTFSourceID))
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			return geneListCheck{}, errBadGeneList{fmt.Errorf(
				"no source %q — pick the GTF the list should resolve through", req.GTFSourceID)}
		}
		return geneListCheck{}, err
	}
	if !gtf.IsGTF() {
		return geneListCheck{}, errBadGeneList{fmt.Errorf(
			"%s is a %s source, not a gene model; a gene list needs a GTF to resolve variants to genes",
			gtf.Ref(), gtf.Kind)}
	}

	available, err := s.catalog.GTFGeneCount(ctx, gtf.ID)
	if err != nil {
		return geneListCheck{}, err
	}
	if available == 0 {
		// Distinguished from "every gene is unknown", which is what it would
		// otherwise look like — and which would send the user off checking gene
		// symbols that are perfectly correct.
		return geneListCheck{}, errBadGeneList{fmt.Errorf(
			"%s has not been scanned for genes yet — provision it (Download) and its "+
				"genes become available for validation when that job finishes", gtf.Ref())}
	}

	genes := catalog.ParseGeneList(req.Genes)
	if len(genes) == 0 {
		return geneListCheck{}, errBadGeneList{errors.New("no genes in the list")}
	}

	unknown, err := s.catalog.UnknownGenes(ctx, gtf.ID, genes, req.byID())
	if err != nil {
		return geneListCheck{}, err
	}
	return geneListCheck{
		GTF:       gtf.Ref(),
		GeneField: fieldName(req.byID()),
		Total:     len(genes),
		Known:     len(genes) - len(unknown),
		Unknown:   unknown,
		Genes:     genes,
		Available: available,
	}, nil
}

func fieldName(byID bool) string {
	if byID {
		return "gene_id"
	}
	return "gene_name"
}

// nameRE bounds what a list may be called: a source name becomes a directory
// name, an id, and an INFO key in a VCF, so it has to survive all three.
var nameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// handleCreateGeneList registers a validated list as an ordinary catalog source.
//
// Refuses on any unknown gene, with no way to override. Marcus, 2026-08-12:
// "Validation needs to be strict — flag bad or missing genes and don't let the
// list be saved until it is corrected." The reason it has to be strict rather
// than advisory: a gene the model does not have contributes nothing at annotate
// time and reports nothing either, so a typo'd symbol is indistinguishable from
// a gene no variant landed in. The list would look like it worked.
func (s *Server) handleCreateGeneList(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	var req geneListRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	name := strings.TrimSpace(req.Name)
	version := strings.TrimSpace(req.Version)
	if name == "" || version == "" {
		writeError(w, http.StatusBadRequest, "a gene list needs a name and a version")
		return
	}
	if !nameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest,
			"the name becomes a VCF INFO key and a directory name: letters, digits and "+
				"underscores only, starting with a letter")
		return
	}
	ann := strings.TrimSpace(req.AnnotationName)
	if ann == "" {
		ann = name
	}
	if !nameRE.MatchString(ann) {
		writeError(w, http.StatusBadRequest,
			"the annotation name becomes a VCF INFO key: letters, digits and underscores "+
				"only, starting with a letter")
		return
	}

	check, err := s.checkGeneList(r, req)
	if err != nil {
		writeGeneListError(w, err)
		return
	}
	if len(check.Unknown) > 0 {
		// The unknown genes come back in the body, not just the message: this is
		// the response the form renders its "fix these" list from, and a caller
		// using the API directly needs the same thing.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("%d of %d genes are not in %s",
				len(check.Unknown), check.Total, check.GTF),
			"check": check,
		})
		return
	}

	gtf, err := s.catalog.GetSource(r.Context(), strings.TrimSpace(req.GTFSourceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	src, err := catalog.SourceFromTOML(catalog.GeneListTOML(catalog.GeneListSpec{
		Name:           name,
		Version:        version,
		Title:          strings.TrimSpace(req.Title),
		Description:    strings.TrimSpace(req.Description),
		GTFRef:         gtf.Ref(),
		Build:          gtf.Build,
		GeneField:      check.GeneField,
		Genes:          check.Genes,
		AnnotationName: ann,
	}))
	if err != nil {
		// The manifest is synthesized here, so this is a bug in the synthesis
		// rather than anything the caller did.
		writeError(w, http.StatusInternalServerError, "generated an invalid manifest: "+err.Error())
		return
	}
	// Closed unless asked otherwise, the same default source registration uses,
	// and for the same reason: the two mistakes are not symmetric. A list that
	// should have been public is one request away from being fixed; one that
	// should have been restricted has already been readable by everyone who could
	// reach the server. A panel can say as much about what a lab is working on as
	// the data behind it.
	src.Visibility = catalog.VisibilityRestricted
	if v := strings.ToLower(strings.TrimSpace(req.Visibility)); catalog.ValidVisibility(v) {
		src.Visibility = v
	}
	// A gene list has no files of its own — it is a set of names plus a reference
	// to a GTF that does — so it is usable the moment it is registered.
	src.IndexStatus = "indexed"

	if err := s.catalog.PutSource(r.Context(), src); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": src.ID, "ref": src.Ref(), "kind": src.Kind,
		"visibility": src.Visibility,
		"gtf":        gtf.Ref(), "genes": len(check.Genes),
	})
}

// handleGetGeneList reads a stored list back into the shape the builder collects,
// so editing one starts from what is there rather than from an empty form.
//
// Reports whether it can be edited and why not, rather than leaving the caller to
// discover it on save. A list a snapshot pins is frozen — a snapshot is a promise
// about what an annotation ran against — and finding that out after retyping a
// panel is the wrong moment.
func (s *Server) handleGetGeneList(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	src, err := s.catalog.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	spec, ok := src.GeneListSpecOf()
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"%s is not an editable gene list — a list whose genes come from a "+
				"genes_file is edited by changing that file", src.Ref()))
		return
	}
	pinned, err := s.catalog.SourceInUse(r.Context(), src.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": src.ID, "ref": src.Ref(),
		"name": spec.Name, "version": spec.Version,
		"title": spec.Title, "description": spec.Description,
		"gtf": spec.GTFRef, "gene_field": fieldName(strings.EqualFold(spec.GeneField, "gene_id")),
		"genes": spec.Genes, "annotation_name": spec.AnnotationName,
		"visibility": src.Visibility,
		"editable":   len(pinned) == 0,
		"pinned_by":  pinned,
	})
}

// handleUpdateGeneList rewrites a list's genes, refusing one a snapshot pins.
//
// Goes through catalog.UpdateSourceTOML rather than PutSource, because changing
// the gene set changes what the source emits: that path refuses a pinned source,
// drops the ad-hoc snapshots built from the old manifest, and purges the cached
// values computed from it — all in one transaction. Writing the manifest alone
// would leave answers cached from the previous gene set, which is not a stale
// cache but a wrong one, and it looks exactly like a correct one.
func (s *Server) handleUpdateGeneList(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	id := r.PathValue("id")
	cur, err := s.catalog.GetSource(r.Context(), id)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !cur.IsGeneList() {
		writeError(w, http.StatusBadRequest, cur.Ref()+" is not a gene list")
		return
	}

	var req geneListRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// Name and version identify the source and are where its files would live, so
	// changing either makes it a different list. Taken from the stored source
	// rather than the request, so a form that echoes them back cannot rename
	// anything by accident.
	ann := strings.TrimSpace(req.AnnotationName)
	if ann == "" {
		ann = cur.Name
	}
	if !nameRE.MatchString(ann) {
		writeError(w, http.StatusBadRequest,
			"the annotation name becomes a VCF INFO key: letters, digits and underscores "+
				"only, starting with a letter")
		return
	}

	check, err := s.checkGeneList(r, req)
	if err != nil {
		writeGeneListError(w, err)
		return
	}
	if len(check.Unknown) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("%d of %d genes are not in %s",
				len(check.Unknown), check.Total, check.GTF),
			"check": check,
		})
		return
	}

	gtf, err := s.catalog.GetSource(r.Context(), strings.TrimSpace(req.GTFSourceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next, err := catalog.SourceFromTOML(catalog.GeneListTOML(catalog.GeneListSpec{
		Name:           cur.Name,
		Version:        cur.Version,
		Title:          strings.TrimSpace(req.Title),
		Description:    strings.TrimSpace(req.Description),
		GTFRef:         gtf.Ref(),
		Build:          gtf.Build,
		GeneField:      check.GeneField,
		Genes:          check.Genes,
		AnnotationName: ann,
	}))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generated an invalid manifest: "+err.Error())
		return
	}
	// Left empty so UpdateSourceTOML carries the stored level forward. Editing a
	// list's genes says nothing about who may use it.
	next.Visibility = ""

	updated, err := s.catalog.UpdateSourceTOML(r.Context(), id, next)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			// Pinned by a snapshot: the caller's to resolve, and the message
			// names the snapshots holding it.
			writeError(w, http.StatusConflict, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": updated.ID, "ref": updated.Ref(),
		"gtf": gtf.Ref(), "genes": len(check.Genes),
	})
}

// handleListGeneModels lists the GTF sources a gene list can be built on, with
// how many genes each has available.
//
// Its own endpoint because the count is the thing that decides whether the form
// can be used at all, and it is not part of a source's identity — it is a fact
// about the cache, which changes when a download job finishes rather than when
// anybody edits the source.
func (s *Server) handleListGeneModels(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog unavailable")
		return
	}
	all, err := s.catalog.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type model struct {
		ID    string `json:"id"`
		Ref   string `json:"ref"`
		Title string `json:"title,omitempty"`
		Build string `json:"build,omitempty"`
		Genes int    `json:"genes" doc:"How many genes are available for validation. Zero means the source has not been provisioned yet."`
	}
	out := []model{}
	for _, src := range all {
		if !src.IsGTF() {
			continue
		}
		n, err := s.catalog.GTFGeneCount(r.Context(), src.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, model{
			ID: src.ID, Ref: src.Ref(), Title: src.Title, Build: src.Build, Genes: n,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// errBadGeneList marks an error the caller can fix, as opposed to one this
// service is responsible for. Without the distinction every one of these reads as
// a 500, and "you have not provisioned that GTF yet" is not a server fault.
type errBadGeneList struct{ error }

func writeGeneListError(w http.ResponseWriter, err error) {
	var bad errBadGeneList
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, bad.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
