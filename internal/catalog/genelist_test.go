package catalog

import (
	"context"
	"strings"
	"testing"
)

const gencodeTOML = `[[sources]]
  format   = "gtf"
  name     = "gencode"
  version  = "48"
  assembly = "GRCh38"
  url      = "https://example.invalid/gencode.v48.gtf.gz"

  [[sources.annotations]]
    name  = "gene"
    field = "GENE"
`

// The list Marcus gave: a small cancer panel, resolved through the GTF above.
const cancerGenesTOML = `[[sources]]
  type       = "genelist"
  name       = "cancer_genes"
  version    = "1"
  gtf        = "gencode:48"
  genes      = ["TP53","MYC","KRAS","MDM2","CDK4","CDKN2B","BRAF","BRCA1","BRCA2","EGFR"]
  gene_field = "gene_name"

  [[sources.annotations]]
    name        = "cancer_gene"
    description = "Variant falls in a cancer-related gene"
`

func geneListFixture(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	for _, src := range []Source{
		{ID: "gencode", Name: "gencode", Version: "48", Kind: "gtf", Build: "GRCh38",
			Visibility: VisibilityPublic, TOML: gencodeTOML},
		{ID: "cancer", Name: "cancer_genes", Version: "1", Kind: "genelist", Build: "GRCh38",
			Visibility: VisibilityPublic, TOML: cancerGenesTOML},
	} {
		if err := s.PutSource(ctx, src); err != nil {
			t.Fatalf("PutSource %s: %v", src.ID, err)
		}
	}
	return s
}

func TestGeneListNamesItsGeneModel(t *testing.T) {
	s := geneListFixture(t)
	ctx := context.Background()

	list, err := s.GetSource(ctx, "cancer")
	if err != nil {
		t.Fatal(err)
	}
	if !list.IsGeneList() {
		t.Fatal("the gene list does not report itself as one")
	}
	if got := list.GeneListGTF(); got != "gencode:48" {
		t.Errorf("GeneListGTF = %q, want gencode:48", got)
	}

	// Anything else has no gene model to name, and must not invent one.
	gtf, err := s.GetSource(ctx, "gencode")
	if err != nil {
		t.Fatal(err)
	}
	if got := gtf.GeneListGTF(); got != "" {
		t.Errorf("a GTF source reported a gene model of its own: %q", got)
	}
}

// varhub resolves the reference by name within the snapshot, so a snapshot
// pinning the list alone loads and then fails every job. Catching it here is the
// difference between an error whoever is assembling it can act on and one that
// reaches the person running the annotation.
func TestSnapshotRefusesAGeneListWithoutItsGeneModel(t *testing.T) {
	s := geneListFixture(t)
	ctx := context.Background()

	err := s.PutSnapshot(ctx, Snapshot{ID: "bad", Build: "GRCh38"}, []string{"cancer"})
	if err == nil {
		t.Fatal("a gene list was pinned without the gene model it resolves through")
	}
	// The message has to name both halves or it is not actionable.
	for _, want := range []string{"cancer_genes:1", "gencode:48"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// With both pinned it is an ordinary snapshot.
	if err := s.PutSnapshot(ctx, Snapshot{ID: "good", Build: "GRCh38"},
		[]string{"cancer", "gencode"}); err != nil {
		t.Fatalf("pinning the list with its gene model was refused: %v", err)
	}
}

// Somebody picking "cancer genes" is asking for the answer, not for a lesson in
// how it is computed — the same reason an ad-hoc selection reaches for the
// assembly's default reference.
func TestAdhocSelectionPinsTheGeneModelForYou(t *testing.T) {
	s := geneListFixture(t)
	ctx := context.Background()

	got, err := s.withGeneListGTF(ctx, []string{"cancer"})
	if err != nil {
		t.Fatalf("withGeneListGTF: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want the list and its gene model", got)
	}
	found := false
	for _, id := range got {
		if id == "gencode" {
			found = true
		}
	}
	if !found {
		t.Errorf("the gene model was not added: %v", got)
	}

	// Already chosen: added once, not twice.
	got, err = s.withGeneListGTF(ctx, []string{"cancer", "gencode"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want no duplicate", got)
	}

	// Nothing to do for a selection with no gene list.
	got, err = s.withGeneListGTF(ctx, []string{"gencode"})
	if err != nil || len(got) != 1 {
		t.Errorf("a selection with no gene list was changed: %v %v", got, err)
	}
}

// The gene model names itself; there is no deployment-wide default to fall back
// on. Saying which source is missing is the whole of the error.
func TestAGeneListWhoseModelIsNotRegistered(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.PutSource(ctx, Source{
		ID: "cancer", Name: "cancer_genes", Version: "1", Kind: "genelist", Build: "GRCh38",
		Visibility: VisibilityPublic, TOML: cancerGenesTOML,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.withGeneListGTF(ctx, []string{"cancer"})
	if err == nil {
		t.Fatal("a gene list with no registered gene model was accepted")
	}
	if !strings.Contains(err.Error(), "gencode:48") {
		t.Errorf("error %q does not name the missing gene model", err)
	}
}

// A bare "gencode" matches on name alone, which is varhub's own rule. Matching
// more strictly here would refuse snapshots varhub accepts.
func TestGeneModelReferenceMatching(t *testing.T) {
	gtf := Source{Name: "gencode", Version: "48", Kind: "gtf"}
	for ref, want := range map[string]bool{
		"gencode":    true,
		"gencode:48": true,
		"gencode:47": false,
		"GENCODE":    true, // varhub resolves by name; case is not the distinction
		"refseq":     false,
	} {
		if got := matchesRef(gtf, ref); got != want {
			t.Errorf("matchesRef(gencode:48, %q) = %v, want %v", ref, got, want)
		}
	}
}
