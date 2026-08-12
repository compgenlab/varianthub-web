package catalog

import (
	"context"
	"strings"
	"testing"
)

func geneCacheStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	if err := s.PutSource(ctx, Source{
		ID: "gencode", Name: "gencode", Version: "48", Kind: "gtf", Build: "GRCh38",
		Visibility: VisibilityPublic, TOML: gencodeTOML,
	}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

var gencodeGenes = []Gene{
	{GeneID: "ENSG00000141510.17", GeneName: "TP53"},
	{GeneID: "ENSG00000157764.13", GeneName: "BRAF"},
	{GeneID: "ENSG00000012048.23", GeneName: "BRCA1"},
	{GeneID: "ENSG00000139618.15", GeneName: "BRCA2"},
}

func TestUnknownGenesReportsOnlyWhatIsMissing(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", gencodeGenes); err != nil {
		t.Fatalf("ReplaceGTFGenes: %v", err)
	}

	missing, err := s.UnknownGenes(ctx, "gencode",
		[]string{"TP53", "NOTAGENE", "BRAF", "ALSONOTAGENE"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 || missing[0] != "NOTAGENE" || missing[1] != "ALSONOTAGENE" {
		t.Errorf("missing = %v, want the two unknown genes in the order given", missing)
	}

	// A list that is entirely known is the case that lets a save through, so it
	// has to be an empty result and not a nil-vs-empty ambiguity.
	missing, err = s.UnknownGenes(ctx, "gencode", []string{"TP53", "BRCA1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("a fully known list reported %v as missing", missing)
	}
}

// Marcus: "Assume all gene names are upper case (both on loading and
// validation)". The user pasting lower case is not making a mistake, and telling
// them tp53 is not a gene would be absurd.
func TestUnknownGenesIsCaseInsensitive(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", gencodeGenes); err != nil {
		t.Fatal(err)
	}
	missing, err := s.UnknownGenes(ctx, "gencode", []string{"tp53", "Braf", "bRcA1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("case-differing spellings reported as unknown: %v", missing)
	}
}

// The whole reason gene ids are stored trimmed. A user pastes ids out of one
// GENCODE release and the GTF is another; the versions differ and the genes do
// not.
func TestUnknownGenesIgnoresGeneIDVersions(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", gencodeGenes); err != nil {
		t.Fatal(err)
	}
	missing, err := s.UnknownGenes(ctx, "gencode", []string{
		"ENSG00000141510",    // no version at all
		"ENSG00000157764.99", // a version this GTF does not have
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("version-differing ids reported as unknown: %v", missing)
	}

	// But a genuinely different id is still missing — trimming must not collapse
	// two genes into one.
	missing, err = s.UnknownGenes(ctx, "gencode", []string{"ENSG00000000000"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Errorf("an unknown gene id was accepted: %v", missing)
	}
}

// The report is shown to the person who typed the list, so it has to name the
// genes the way they typed them — "Brca9", not "BRCA9".
func TestUnknownGenesReportsTheSpellingTheUserTyped(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", gencodeGenes); err != nil {
		t.Fatal(err)
	}
	missing, err := s.UnknownGenes(ctx, "gencode", []string{"Brca9"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "Brca9" {
		t.Errorf("missing = %v, want the spelling as typed", missing)
	}
}

// A gene the new GTF dropped has to stop validating, or a list saves against a
// gene the annotation run can no longer find.
func TestReplaceGTFGenesReplaces(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", gencodeGenes); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceGTFGenes(ctx, "gencode", []Gene{
		{GeneID: "ENSG00000141510.18", GeneName: "TP53"},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.GTFGeneCount(ctx, "gencode")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("gene count = %d after replacing four genes with one", n)
	}
	missing, err := s.UnknownGenes(ctx, "gencode", []string{"BRAF"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Errorf("a gene the new GTF does not have still validates")
	}
}

// Trimming versions happens after varhub has already deduplicated by the raw id,
// so two rows can collide on the way in — and CopyFrom has no ON CONFLICT to
// absorb it. Without dedup here the whole scan fails on a primary-key violation.
func TestReplaceGTFGenesToleratesIDsThatCollideAfterTrimming(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", []Gene{
		{GeneID: "ENSG00000141510.17", GeneName: "TP53"},
		{GeneID: "ENSG00000141510.18", GeneName: "TP53"},
	}); err != nil {
		t.Fatalf("colliding ids failed the whole scan: %v", err)
	}
	n, err := s.GTFGeneCount(ctx, "gencode")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("gene count = %d, want the two versions stored as one gene", n)
	}
}

// A symbol is not unique in a GTF — GENCODE's PAR regions carry the same name
// under two ids. Keying on the name would drop one, and which one would depend
// on file order.
func TestReplaceGTFGenesKeepsTwoGenesSharingAName(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", []Gene{
		{GeneID: "ENSG00000182378.14", GeneName: "PLCXD1"},
		{GeneID: "ENSG00000182378.14_PAR_Y", GeneName: "PLCXD1"},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.GTFGeneCount(ctx, "gencode")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("gene count = %d, want both genes sharing the symbol", n)
	}
	// And the shared symbol still validates once, not twice.
	missing, err := s.UnknownGenes(ctx, "gencode", []string{"PLCXD1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("PLCXD1 reported missing: %v", missing)
	}
}

// Zero rows means "not scanned yet", which the UI has to be able to tell from a
// GTF whose genes are all unknown to the list.
func TestGTFGeneCountIsZeroBeforeAnyScan(t *testing.T) {
	s, ctx := geneCacheStore(t)
	n, err := s.GTFGeneCount(ctx, "gencode")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("gene count = %d before any scan", n)
	}
}

// Removing a source has to take its gene cache with it, or the rows outlive the
// thing they describe and the next source to reuse the id inherits them.
func TestDeletingASourceDropsItsGenes(t *testing.T) {
	s, ctx := geneCacheStore(t)
	if err := s.ReplaceGTFGenes(ctx, "gencode", gencodeGenes); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeleteSource(ctx, "gencode"); err != nil {
		t.Fatal(err)
	}
	n, err := s.GTFGeneCount(ctx, "gencode")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d gene rows outlived their source", n)
	}
}

func TestParseGeneList(t *testing.T) {
	// The shapes a list actually arrives in: a pasted spreadsheet column, a
	// comma-joined list out of a paper, a space-separated line out of a terminal.
	for _, in := range []string{
		"TP53\nMYC\nKRAS",
		"TP53, MYC, KRAS",
		"TP53\tMYC\tKRAS",
		"TP53 MYC KRAS",
		"  TP53 ,\n\n MYC ;KRAS  ",
		"tp53\nmyc\nkras",
		"\"TP53\",\"MYC\",\"KRAS\"",
	} {
		got := ParseGeneList(in)
		want := []string{"TP53", "MYC", "KRAS"}
		if len(got) != len(want) {
			t.Errorf("ParseGeneList(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("ParseGeneList(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

func TestParseGeneListDedupesAndSkipsComments(t *testing.T) {
	got := ParseGeneList("# my panel\nTP53\ntp53\nTP53\nMYC")
	if len(got) != 2 || got[0] != "TP53" || got[1] != "MYC" {
		t.Errorf("ParseGeneList = %v, want [TP53 MYC]", got)
	}
	if strings.Join(ParseGeneList("\n\n  \n"), ",") != "" {
		t.Error("an empty paste produced genes")
	}
}
