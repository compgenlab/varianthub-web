package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/catalog"
)

const geneModelTOML = `[[sources]]
  format   = "gtf"
  name     = "gencode"
  version  = "48"
  assembly = "GRCh38"
  url      = "https://example.invalid/gencode.v48.gtf.gz"

  [[sources.annotations]]
    name  = "gene"
    field = "GENE"
`

// The panel Marcus gave, plus the ids GENCODE would carry for them.
var panelGenes = []catalog.Gene{
	{GeneID: "ENSG00000141510.17", GeneName: "TP53"},
	{GeneID: "ENSG00000136997.20", GeneName: "MYC"},
	{GeneID: "ENSG00000133703.13", GeneName: "KRAS"},
	{GeneID: "ENSG00000135679.25", GeneName: "MDM2"},
	{GeneID: "ENSG00000135446.18", GeneName: "CDK4"},
	{GeneID: "ENSG00000147883.13", GeneName: "CDKN2B"},
	{GeneID: "ENSG00000157764.14", GeneName: "BRAF"},
	{GeneID: "ENSG00000012048.23", GeneName: "BRCA1"},
	{GeneID: "ENSG00000139618.15", GeneName: "BRCA2"},
	{GeneID: "ENSG00000146648.20", GeneName: "EGFR"},
}

const panelPaste = "TP53, MYC, KRAS, MDM2, CDK4, CDKN2B, BRAF, BRCA1, BRCA2, EGFR"

// geneListHarness registers a scanned GTF source and returns an admin session.
func geneListHarness(t *testing.T, scanned bool) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()

	if err := h.cat.PutSource(ctx, catalog.Source{
		ID: "gencode", Name: "gencode", Version: "48", Kind: "gtf", Build: "GRCh38",
		Visibility: catalog.VisibilityPublic, TOML: geneModelTOML,
	}); err != nil {
		t.Fatal(err)
	}
	if scanned {
		if err := h.cat.ReplaceGTFGenes(ctx, "gencode", panelGenes); err != nil {
			t.Fatal(err)
		}
	}
	return h, h.session(t)
}

func decodeCheck(t *testing.T, body []byte) geneListCheck {
	t.Helper()
	var c geneListCheck
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode check: %v (%s)", err, body)
	}
	return c
}

func TestValidateGeneListAcceptsAKnownPanel(t *testing.T) {
	h, sess := geneListHarness(t, true)

	got := h.doSession("POST", "/api/v1/admin/genelists/validate", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: panelPaste,
	})
	if got.Code != http.StatusOK {
		t.Fatalf("validate = %d: %s", got.Code, got.Body)
	}
	c := decodeCheck(t, got.Body.Bytes())
	if c.Total != 10 || c.Known != 10 || len(c.Unknown) != 0 {
		t.Errorf("check = %+v, want all ten known", c)
	}
	if c.GTF != "gencode:48" || c.GeneField != "gene_name" {
		t.Errorf("check reported gtf %q field %q", c.GTF, c.GeneField)
	}
	if c.Available != len(panelGenes) {
		t.Errorf("available = %d, want %d", c.Available, len(panelGenes))
	}
}

func TestValidateGeneListNamesTheGenesItDoesNotKnow(t *testing.T) {
	h, sess := geneListHarness(t, true)

	got := h.doSession("POST", "/api/v1/admin/genelists/validate", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: "TP53\nP53\nBRAF\nBRCA5",
	})
	if got.Code != http.StatusOK {
		t.Fatalf("validate = %d: %s", got.Code, got.Body)
	}
	c := decodeCheck(t, got.Body.Bytes())
	if len(c.Unknown) != 2 {
		t.Fatalf("unknown = %v, want the two that are not genes", c.Unknown)
	}
	// P53 is the case this feature exists for: a perfectly reasonable thing to
	// type that is not the symbol. Aliases are the user's problem by decision, so
	// the only help offered is naming it precisely.
	if c.Unknown[0] != "P53" || c.Unknown[1] != "BRCA5" {
		t.Errorf("unknown = %v, want [P53 BRCA5] in the order given", c.Unknown)
	}
	if c.Known != 2 {
		t.Errorf("known = %d, want 2", c.Known)
	}
}

// A GTF nobody has provisioned has no genes, and every gene in the list would
// come back unknown — sending the user off to check symbols that are correct.
func TestValidateGeneListSaysWhenTheGTFHasNotBeenScanned(t *testing.T) {
	h, sess := geneListHarness(t, false)

	got := h.doSession("POST", "/api/v1/admin/genelists/validate", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: panelPaste,
	})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("validate = %d, want 400: %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "not been scanned") {
		t.Errorf("error %s does not explain that the GTF has no genes yet", got.Body)
	}
}

func TestValidateGeneListRefusesANonGTFSource(t *testing.T) {
	h, sess := geneListHarness(t, true)
	ctx := context.Background()
	if err := h.cat.PutSource(ctx, catalog.Source{
		ID: "clinvar", Name: "clinvar", Version: "2025", Kind: "vcf", Build: "GRCh38",
		Visibility: catalog.VisibilityPublic, TOML: "[[sources]]\n  name = \"clinvar\"\n  version = \"2025\"\n  format = \"vcf\"\n",
	}); err != nil {
		t.Fatal(err)
	}
	got := h.doSession("POST", "/api/v1/admin/genelists/validate", sess, geneListRequest{
		GTFSourceID: "clinvar", Genes: panelPaste,
	})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("validate = %d, want 400: %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "gene model") {
		t.Errorf("error %s does not say a GTF is needed", got.Body)
	}
}

func TestCreateGeneListRegistersAnOrdinarySource(t *testing.T) {
	h, sess := geneListHarness(t, true)

	got := h.doSession("POST", "/api/v1/admin/genelists", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: panelPaste,
		Name: "cancer_genes", Version: "1", Title: "Cancer panel",
		AnnotationName: "cancer_gene",
	})
	if got.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", got.Code, got.Body)
	}

	src, err := h.cat.GetSource(context.Background(), "cancer_genes-1")
	if err != nil {
		t.Fatalf("the created list is not in the catalog: %v", err)
	}
	// The whole design: what comes out is an ordinary source, so everything
	// already built — the picker, snapshots, grants, the cache — applies without
	// knowing it was generated.
	if !src.IsGeneList() {
		t.Errorf("kind = %q, want genelist", src.Kind)
	}
	if src.GeneListGTF() != "gencode:48" {
		t.Errorf("the list does not name its gene model: %q", src.GeneListGTF())
	}
	if src.Build != "GRCh38" {
		t.Errorf("build = %q, want the GTF's assembly", src.Build)
	}
	// Private unless asked otherwise, matching source registration.
	if src.Visibility != catalog.VisibilityRestricted {
		t.Errorf("visibility = %q, want private by default", src.Visibility)
	}
	for _, want := range []string{`gene_field = "gene_name"`, `"TP53"`, `name        = "cancer_gene"`} {
		if !strings.Contains(src.TOML, want) {
			t.Errorf("manifest does not contain %s:\n%s", want, src.TOML)
		}
	}
}

// The strictness is the feature. A gene the model does not have contributes
// nothing at annotate time and reports nothing either, so a typo is
// indistinguishable from a gene no variant landed in — the list looks like it
// worked.
func TestCreateGeneListRefusesAnUnknownGene(t *testing.T) {
	h, sess := geneListHarness(t, true)

	got := h.doSession("POST", "/api/v1/admin/genelists", sess, geneListRequest{
		GTFSourceID: "gencode", Genes: "TP53\nNOTAGENE",
		Name: "cancer_genes", Version: "1",
	})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400: %s", got.Code, got.Body)
	}
	// The unknown genes come back in the body, not only in the message: the form
	// renders its "fix these" list from this.
	var body struct {
		Error string        `json:"error"`
		Check geneListCheck `json:"check"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Check.Unknown) != 1 || body.Check.Unknown[0] != "NOTAGENE" {
		t.Errorf("unknown = %v, want [NOTAGENE]", body.Check.Unknown)
	}
	if _, err := h.cat.GetSource(context.Background(), "cancer_genes-1"); err == nil {
		t.Error("a list with an unknown gene was saved anyway")
	}
}

// A name becomes a directory, an id and a VCF INFO key, so it has to survive all
// three. Rejecting at the door beats a manifest that fails to load later.
func TestCreateGeneListRejectsUnusableNames(t *testing.T) {
	h, sess := geneListHarness(t, true)

	for _, tc := range []struct{ name, ann string }{
		{"cancer genes", ""}, // a space
		{"cancer-genes", ""}, // a dash: fine in a path, not in an INFO key
		{"2fast", ""},        // leading digit
		{"", ""},             // nothing
		{"ok", "bad name"},   // the annotation key is the same constraint
		{"ok", "semi;colon"}, // and would corrupt a VCF INFO column outright
	} {
		got := h.doSession("POST", "/api/v1/admin/genelists", sess, geneListRequest{
			GTFSourceID: "gencode", Genes: "TP53",
			Name: tc.name, Version: "1", AnnotationName: tc.ann,
		})
		if got.Code != http.StatusBadRequest {
			t.Errorf("name %q ann %q = %d, want 400", tc.name, tc.ann, got.Code)
		}
	}
}

// Ids and names are two different questions of the same table, and a list built
// on ids has to validate against ids.
func TestCreateGeneListByGeneID(t *testing.T) {
	h, sess := geneListHarness(t, true)

	got := h.doSession("POST", "/api/v1/admin/genelists", sess, geneListRequest{
		GTFSourceID: "gencode", GeneField: "gene_id",
		// Versionless, against a model whose ids are versioned. This is the case
		// the trimming exists for.
		Genes: "ENSG00000141510\nENSG00000157764",
		Name:  "by_id", Version: "1",
	})
	if got.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", got.Code, got.Body)
	}
	src, err := h.cat.GetSource(context.Background(), "by_id-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src.TOML, `gene_field = "gene_id"`) {
		t.Errorf("manifest does not match on gene_id:\n%s", src.TOML)
	}
	// Stored trimmed, which is what varhub will compare against.
	if !strings.Contains(src.TOML, `"ENSG00000141510"`) {
		t.Errorf("manifest does not carry the trimmed id:\n%s", src.TOML)
	}
}

// The endpoints are the web app's, not the published API's — the same split every
// other admin route follows. A token gets 404 rather than 403, so the surface a
// token can see does not admit that these exist.
func TestGeneListEndpointsAreNotOnTheTokenSurface(t *testing.T) {
	h, _ := geneListHarness(t, true)
	_, tok := h.admin(t)

	for _, path := range []string{
		"/api/v1/admin/genelists",
		"/api/v1/admin/genelists/validate",
	} {
		if got := h.do("POST", path, tok, geneListRequest{}); got.Code != http.StatusNotFound {
			t.Errorf("POST %s with a token = %d, want 404", path, got.Code)
		}
	}
	if got := h.do("GET", "/api/v1/admin/genelists/models", tok, nil); got.Code != http.StatusNotFound {
		t.Errorf("GET models with a token = %d, want 404", got.Code)
	}
}

// The form cannot be used at all against an unscanned GTF, so the picker has to
// report the count rather than only the name.
func TestListGeneModelsReportsWhatIsAvailable(t *testing.T) {
	h, sess := geneListHarness(t, true)

	got := h.doSession("GET", "/api/v1/admin/genelists/models", sess, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("models = %d: %s", got.Code, got.Body)
	}
	var models []struct {
		ID    string `json:"id"`
		Ref   string `json:"ref"`
		Genes int    `json:"genes"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gencode" {
		t.Fatalf("models = %+v, want the one GTF", models)
	}
	if models[0].Genes != len(panelGenes) {
		t.Errorf("genes = %d, want %d", models[0].Genes, len(panelGenes))
	}
}
