package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

type storedList struct {
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	GTF            string   `json:"gtf"`
	GeneField      string   `json:"gene_field"`
	Genes          []string `json:"genes"`
	AnnotationName string   `json:"annotation_name"`
	Visibility     string   `json:"visibility"`
	Editable       bool     `json:"editable"`
	PinnedBy       []string `json:"pinned_by"`
}

// makeList creates one through the ordinary endpoint, so the thing being edited
// is the thing the builder actually produces.
func makeList(t *testing.T, h *harness, sess, name, genes string) {
	t.Helper()
	got := h.doSession("POST", "/api/v1/admin/genelists", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: genes, Name: name, Version: "1",
		Visibility: catalog.VisibilityPublic,
	})
	if got.Code != http.StatusOK {
		t.Fatalf("create %s = %d: %s", name, got.Code, got.Body)
	}
}

func readList(t *testing.T, h *harness, sess, id string) storedList {
	t.Helper()
	got := h.doSession("GET", "/api/v1/admin/genelists/"+id, sess, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("read %s = %d: %s", id, got.Code, got.Body)
	}
	var out storedList
	if err := json.Unmarshal(got.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Editing starts from what is stored, so the form can be filled in rather than
// retyped. The genes come back as a list, not the manifest text — the point is
// that a caller never has to parse TOML.
func TestGeneListReadsBackIntoTheBuildersShape(t *testing.T) {
	h, sess := geneListHarness(t, true)
	makeList(t, h, sess, "panel", "TP53, BRAF")

	got := readList(t, h, sess, "panel-1")
	if len(got.Genes) != 2 || got.Genes[0] != "BRAF" || got.Genes[1] != "TP53" {
		t.Errorf("genes = %v, want the stored pair, sorted", got.Genes)
	}
	if got.GTF != "gencode:48" || got.GeneField != "gene_name" {
		t.Errorf("gtf %q field %q", got.GTF, got.GeneField)
	}
	if got.Name != "panel" || got.Version != "1" {
		t.Errorf("name %q version %q", got.Name, got.Version)
	}
	if !got.Editable || len(got.PinnedBy) != 0 {
		t.Errorf("a list no snapshot pins reported editable=%v pinned_by=%v",
			got.Editable, got.PinnedBy)
	}
}

func TestUpdateGeneListRewritesTheGenes(t *testing.T) {
	h, sess := geneListHarness(t, true)
	makeList(t, h, sess, "panel", "TP53, BRAF")

	got := h.doSession("PUT", "/api/v1/admin/genelists/panel-1", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: "TP53\nEGFR", Title: "Edited",
	})
	if got.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", got.Code, got.Body)
	}
	after := readList(t, h, sess, "panel-1")
	if len(after.Genes) != 2 || after.Genes[0] != "EGFR" || after.Genes[1] != "TP53" {
		t.Errorf("genes = %v, want [EGFR TP53]", after.Genes)
	}
	if after.Title != "Edited" {
		t.Errorf("title = %q", after.Title)
	}
	// Changing which genes are in a list says nothing about who may use it.
	if after.Visibility != catalog.VisibilityPublic {
		t.Errorf("visibility = %q after an edit, want it carried forward", after.Visibility)
	}
}

// The rule the user asked for, and the one UpdateSourceTOML already enforces: a
// snapshot is a promise about what an annotation ran against.
func TestUpdateGeneListRefusesOneASnapshotPins(t *testing.T) {
	h, sess := geneListHarness(t, true)
	ctx := context.Background()
	makeList(t, h, sess, "panel", "TP53, BRAF")
	if err := h.cat.PutSnapshot(ctx,
		catalog.Snapshot{ID: "snap", Build: "GRCh38"},
		[]string{"panel-1", "gencode"}); err != nil {
		t.Fatal(err)
	}

	// Reported before the form is touched...
	before := readList(t, h, sess, "panel-1")
	if before.Editable {
		t.Error("a pinned list reported itself editable")
	}
	if len(before.PinnedBy) != 1 || before.PinnedBy[0] != "snap" {
		t.Errorf("pinned_by = %v, want the snapshot holding it", before.PinnedBy)
	}

	// ...and refused if attempted anyway.
	got := h.doSession("PUT", "/api/v1/admin/genelists/panel-1", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: "TP53",
	})
	if got.Code != http.StatusConflict {
		t.Fatalf("update of a pinned list = %d, want 409: %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "snap") {
		t.Errorf("error %s does not name the snapshot", got.Body)
	}
	after := readList(t, h, sess, "panel-1")
	if len(after.Genes) != 2 {
		t.Errorf("the refused edit changed the list anyway: %v", after.Genes)
	}
}

// Strictness is the same on the way in as on creation: an edit is how a typo
// gets fixed, so it must not be how one gets introduced.
func TestUpdateGeneListStillValidatesStrictly(t *testing.T) {
	h, sess := geneListHarness(t, true)
	makeList(t, h, sess, "panel", "TP53, BRAF")

	got := h.doSession("PUT", "/api/v1/admin/genelists/panel-1", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: "TP53\nNOTAGENE",
	})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("update = %d, want 400: %s", got.Code, got.Body)
	}
	var body struct {
		Check geneListCheck `json:"check"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Check.Unknown) != 1 || body.Check.Unknown[0] != "NOTAGENE" {
		t.Errorf("unknown = %v", body.Check.Unknown)
	}
	if after := readList(t, h, sess, "panel-1"); len(after.Genes) != 2 {
		t.Errorf("the refused edit changed the list: %v", after.Genes)
	}
}

// Name and version identify the source and are where its files would live, so an
// edit cannot change them — a form echoing them back must not be able to rename
// anything by accident.
func TestUpdateGeneListCannotRename(t *testing.T) {
	h, sess := geneListHarness(t, true)
	makeList(t, h, sess, "panel", "TP53")

	got := h.doSession("PUT", "/api/v1/admin/genelists/panel-1", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: "TP53",
		Name: "something_else", Version: "9",
	})
	if got.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", got.Code, got.Body)
	}
	after := readList(t, h, sess, "panel-1")
	if after.Name != "panel" || after.Version != "1" {
		t.Errorf("renamed to %s:%s", after.Name, after.Version)
	}
	if _, err := h.cat.GetSource(context.Background(), "something_else-9"); err == nil {
		t.Error("the edit created a second source under the requested name")
	}
}

// "models" is a literal path segment sharing a prefix with "{id}". Go's mux
// prefers the literal, but nothing in the code says so — and a rename that broke
// it would fail as a confusing 400 about a gene list called "models".
func TestGeneListModelsIsNotShadowedByTheIDRoute(t *testing.T) {
	h, sess := geneListHarness(t, true)

	got := h.doSession("GET", "/api/v1/admin/genelists/models", sess, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("models = %d: %s", got.Code, got.Body)
	}
	var models []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &models); err != nil {
		t.Fatalf("models did not return the model list: %v (%s)", err, got.Body)
	}
	if len(models) != 1 || models[0].ID != "gencode" {
		t.Errorf("models = %+v", models)
	}
}

// A list whose genes live in a genes_file has nothing for the form to edit, and
// saving would silently drop them. Refused with the reason rather than opened
// with an empty box.
func TestAGeneListBackedByAFileIsNotEditableHere(t *testing.T) {
	h, sess := geneListHarness(t, true)
	ctx := context.Background()
	if err := h.cat.PutSource(ctx, catalog.Source{
		ID: "filed", Name: "filed", Version: "1", Kind: "genelist", Build: "GRCh38",
		Visibility: catalog.VisibilityPublic,
		TOML: "[[sources]]\n  type = \"genelist\"\n  name = \"filed\"\n  version = \"1\"\n" +
			"  gtf = \"gencode:48\"\n  genes_file = \"panel.txt\"\n\n" +
			"  [[sources.annotations]]\n    name = \"filed\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	got := h.doSession("GET", "/api/v1/admin/genelists/filed", sess, nil)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("read = %d, want 400: %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "genes_file") {
		t.Errorf("error %s does not say why", got.Body)
	}
}
