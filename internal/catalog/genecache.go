package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/compgenlab/cghts/gtf"
)

// Gene is one gene's identity, as a GTF source reports it.
type Gene struct {
	GeneID   string `json:"gene_id"`
	GeneName string `json:"gene_name"`
}

// GeneKey normalizes a gene identifier to the form membership is tested in.
//
// The same rule varhub applies when a gene list annotates — upper case, and for
// ids the Ensembl/GENCODE version suffix removed — because a list validated here
// has to match there. It is duplicated rather than shared only in the sense that
// the trimming itself comes from cghts, which both sides import; what is repeated
// is the decision to apply it, and the tests on both sides pin the same examples.
//
// See varhub's config.GeneKey.
func GeneKey(g string, byID bool) string {
	g = strings.TrimSpace(g)
	if byID {
		g = gtf.TrimGeneIDVersion(g)
	}
	return strings.ToUpper(g)
}

// ReplaceGTFGenes stores the genes a GTF source knows, replacing whatever was
// there.
//
// Replace rather than merge: this is a cache of what the file says now, and a
// gene the new GTF dropped must stop validating. Merging would leave a list
// passing validation against a gene the annotation run can no longer find, which
// is the one failure this table exists to prevent.
//
// In one transaction, so a source is never half-populated — a partial table
// looks exactly like a GTF that is missing genes, and would reject a correct
// list with no indication why.
func (s *Store) ReplaceGTFGenes(ctx context.Context, sourceID string, genes []Gene) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM gtf_gene WHERE source_id = $1`, sourceID); err != nil {
		return fmt.Errorf("clear genes for %s: %w", sourceID, err)
	}

	// CopyFrom rather than a batch of inserts: this is ~78k rows for GENCODE, and
	// the binary copy protocol is the difference between a second and a minute.
	//
	// Deduplicated here rather than trusted from the caller, and by the key the
	// table is keyed on. varhub's scan already reports each gene_id once, but this
	// trims versions afterwards — so two ids that differed only by version arrive
	// as one, and CopyFrom has no ON CONFLICT to absorb that.
	seen := make(map[string]bool, len(genes))
	rows := make([][]any, 0, len(genes))
	for _, g := range genes {
		id := GeneKey(g.GeneID, true)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		rows = append(rows, []any{sourceID, id, GeneKey(g.GeneName, false)})
	}

	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"gtf_gene"},
		[]string{"source_id", "gene_id", "gene_name"},
		pgx.CopyFromRows(rows),
	); err != nil {
		return fmt.Errorf("store genes for %s: %w", sourceID, err)
	}
	return tx.Commit(ctx)
}

// GTFGeneCount reports how many genes are cached for a source. Zero means the
// GTF has not been scanned yet, which is a different thing from a GTF with no
// genes and has to be reported differently.
func (s *Store) GTFGeneCount(ctx context.Context, sourceID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM gtf_gene WHERE source_id = $1`, sourceID).Scan(&n)
	return n, err
}

// UnknownGenes returns the entries of names that the source has no gene for, in
// the order they were given, deduplicated.
//
// Returns what is missing rather than what matched, because that is the whole of
// the answer a strict validation needs: the caller either gets an empty slice and
// saves, or gets a list to show the user. Reporting the matches too would invite
// a caller to save the subset that matched, which is exactly the "save anyway"
// behaviour this feature deliberately does not have.
func (s *Store) UnknownGenes(ctx context.Context, sourceID string, names []string, byID bool) ([]string, error) {
	// Normalized keys, and the spelling the user typed for each, so the report
	// names genes the way they wrote them rather than the way we store them.
	var order []string
	typed := map[string]string{}
	for _, n := range names {
		k := GeneKey(n, byID)
		if k == "" {
			continue
		}
		if _, ok := typed[k]; ok {
			continue
		}
		typed[k] = strings.TrimSpace(n)
		order = append(order, k)
	}
	if len(order) == 0 {
		return nil, nil
	}

	col := "gene_name"
	if byID {
		col = "gene_id"
	}
	// One query against the whole list. The alternative — a query per gene — is
	// hundreds of round trips for a panel, on a form submit.
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT `+col+` FROM gtf_gene WHERE source_id = $1 AND `+col+` = ANY($2)`,
		sourceID, order)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		found[g] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var missing []string
	for _, k := range order {
		if !found[k] {
			missing = append(missing, typed[k])
		}
	}
	return missing, nil
}

// ParseGeneList splits pasted text into gene identifiers.
//
// Deliberately permissive about the separator — newlines, commas, tabs, spaces,
// semicolons all work — because the text comes from wherever the user had it: a
// column pasted out of a spreadsheet, a comma-joined list out of a paper, a
// space-separated line out of a terminal. Being strict about the separator would
// reject correct lists for a reason that has nothing to do with genes.
//
// Strict about the genes themselves, though: whatever survives the split is
// validated as written, and a stray token is reported as an unknown gene rather
// than quietly dropped.
//
// A "#" comments out the rest of its line, matching varhub's genes_file, because
// the text people paste is often the contents of one.
func ParseGeneList(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		// Line-wise, before splitting: a comment runs to the end of its line, and
		// splitting first would scatter its words into the gene list.
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			switch r {
			case '\r', '\t', ' ', ',', ';', '|':
				return true
			}
			return false
		}) {
			f = strings.Trim(strings.TrimSpace(f), `"'`)
			if f == "" {
				continue
			}
			up := strings.ToUpper(f)
			if seen[up] {
				continue
			}
			seen[up] = true
			out = append(out, up)
		}
	}
	return out
}

// SortedGeneNames is a stable rendering of a gene set for a manifest.
//
// Sorted so the TOML a list produces depends only on which genes are in it, not
// on the order they were pasted — two admins entering the same panel get the same
// manifest, and re-saving one without changing it produces no diff.
func SortedGeneNames(genes []string) []string {
	out := append([]string{}, genes...)
	sort.Strings(out)
	return out
}

// GeneListSpec is a gene list as the builder collects it, before it becomes a
// manifest.
type GeneListSpec struct {
	Name        string
	Version     string
	Title       string
	Description string

	// GTFRef is the gene model, as "name:version". Written into the manifest
	// verbatim: varhub resolves it by name within the snapshot, which is why
	// PutSnapshot refuses a list whose GTF is not pinned alongside it.
	GTFRef string
	// Build is the assembly, which must be the GTF's. A snapshot's sources have
	// to agree on one or varhub rejects the whole manifest.
	Build string

	// GeneField is "gene_name" (default) or "gene_id".
	GeneField string
	Genes     []string

	// AnnotationName is the INFO key a match writes. Defaults to Name.
	AnnotationName string
}

// GeneListTOML renders the spec as a varhub source manifest.
//
// Synthesized rather than hand-written because the point of the builder is that
// nobody has to know this shape — but the result is an ordinary manifest, stored
// as text like every other, so a list made this way can be read, edited and
// re-registered exactly like one that was written by hand. Nothing downstream
// needs to know it was generated.
func GeneListTOML(spec GeneListSpec) string {
	ann := spec.AnnotationName
	if ann == "" {
		ann = spec.Name
	}
	field := spec.GeneField
	if field == "" {
		field = "gene_name"
	}
	desc := spec.Description
	if desc == "" {
		desc = "Variant falls in a listed gene"
	}

	var b strings.Builder
	b.WriteString("[[sources]]\n")
	b.WriteString("  type       = \"genelist\"\n")
	fmt.Fprintf(&b, "  name       = %s\n", tomlString(spec.Name))
	fmt.Fprintf(&b, "  version    = %s\n", tomlString(spec.Version))
	if spec.Title != "" {
		fmt.Fprintf(&b, "  title      = %s\n", tomlString(spec.Title))
	}
	if spec.Description != "" {
		fmt.Fprintf(&b, "  desc       = %s\n", tomlString(spec.Description))
	}
	if spec.Build != "" {
		fmt.Fprintf(&b, "  assembly   = %s\n", tomlString(spec.Build))
	}
	fmt.Fprintf(&b, "  gtf        = %s\n", tomlString(spec.GTFRef))
	fmt.Fprintf(&b, "  gene_field = %s\n", tomlString(field))

	// One gene per line rather than a single long array. A panel is hundreds of
	// entries, and this manifest is shown to admins in the source config view and
	// diffed when somebody edits it — a 4 KB line is unreadable in both.
	b.WriteString("  genes      = [\n")
	for _, g := range SortedGeneNames(spec.Genes) {
		fmt.Fprintf(&b, "    %s,\n", tomlString(g))
	}
	b.WriteString("  ]\n")

	b.WriteString("\n  [[sources.annotations]]\n")
	fmt.Fprintf(&b, "    name        = %s\n", tomlString(ann))
	fmt.Fprintf(&b, "    description = %s\n", tomlString(desc))
	return b.String()
}
