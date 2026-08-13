package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// GeneListGTF is the GTF source a gene-list source resolves variants through,
// as "name" or "name:version". Empty for anything that is not a gene list.
//
// A gene list flags a variant when the gene overlapping it is in a named set,
// which means it cannot answer anything without a gene model. varhub resolves
// that reference by name *within the snapshot*, so a gene list pinned without
// its GTF is not a degraded snapshot — it is one that fails at annotate time
// with "gtf X is not a GTF source in this snapshot".
//
// Read from toml_text like the rest of the projection, so a source registered
// before this existed needs no backfill.
func (s Source) GeneListGTF() string {
	if !s.IsGeneList() {
		return ""
	}
	var f struct {
		Sources []struct {
			GTF string `toml:"gtf"`
		} `toml:"sources"`
	}
	if _, err := toml.Decode(s.TOML, &f); err != nil || len(f.Sources) == 0 {
		return ""
	}
	return strings.TrimSpace(f.Sources[0].GTF)
}

// IsGeneList reports whether the source flags variants by gene membership.
func (s Source) IsGeneList() bool { return s.Kind == "genelist" }

// GeneListSpecOf reads a stored gene list back into the shape the builder
// collects, so an existing list can be edited rather than rewritten from
// scratch.
//
// Parsed from toml_text like GeneListGTF, rather than from a projection: the
// manifest is the source of truth, and a list registered by hand on the sources
// page is as editable here as one the builder made. That is the whole reason
// generated manifests are ordinary text — nothing downstream knows which is
// which, and this must not either.
func (s Source) GeneListSpecOf() (GeneListSpec, bool) {
	if !s.IsGeneList() {
		return GeneListSpec{}, false
	}
	var f struct {
		Sources []struct {
			Name      string   `toml:"name"`
			Version   string   `toml:"version"`
			Title     string   `toml:"title"`
			Desc      string   `toml:"desc"`
			Assembly  string   `toml:"assembly"`
			GTF       string   `toml:"gtf"`
			GeneField string   `toml:"gene_field"`
			Genes     []string `toml:"genes"`
			// genes_file is not read: the builder has no file to point at, and
			// silently dropping it on save would delete genes the manifest
			// declared. A list using one is reported as not editable here.
			GenesFile   string `toml:"genes_file"`
			Annotations []struct {
				Name        string `toml:"name"`
				Description string `toml:"description"`
			} `toml:"annotations"`
		} `toml:"sources"`
	}
	if _, err := toml.Decode(s.TOML, &f); err != nil || len(f.Sources) == 0 {
		return GeneListSpec{}, false
	}
	src := f.Sources[0]
	if src.GenesFile != "" {
		return GeneListSpec{}, false
	}
	spec := GeneListSpec{
		Name:        src.Name,
		Version:     src.Version,
		Title:       src.Title,
		Description: src.Desc,
		GTFRef:      strings.TrimSpace(src.GTF),
		Build:       src.Assembly,
		GeneField:   strings.TrimSpace(src.GeneField),
		Genes:       src.Genes,
	}
	if len(src.Annotations) > 0 {
		spec.AnnotationName = src.Annotations[0].Name
		if spec.Description == "" {
			spec.Description = src.Annotations[0].Description
		}
	}
	return spec, true
}

// matchesRef reports whether src is the source a "name" or "name:version"
// reference names.
//
// A bare name matches on name alone, which is varhub's own rule: it resolves the
// reference by name and then checks the version if one was given. Matching
// stricter here would refuse snapshots varhub accepts.
func matchesRef(src Source, ref string) bool {
	name, version := ref, ""
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		name, version = ref[:i], ref[i+1:]
	}
	if !strings.EqualFold(src.Name, name) {
		return false
	}
	return version == "" || src.Version == version
}

// missingGeneListGTF names the gene lists in a set whose GTF is not also in it,
// each with the reference it wanted.
//
// Returned rather than resolved, because the two callers want different things:
// saving a snapshot should refuse and say what to add, and an ad-hoc selection
// should add it.
func missingGeneListGTF(chosen []Source) map[string]string {
	out := map[string]string{}
	for _, src := range chosen {
		if !src.IsGeneList() {
			continue
		}
		want := src.GeneListGTF()
		if want == "" {
			// varhub's own validation reports this precisely at load; repeating
			// the check here would be a second opinion about a manifest this
			// service does not parse.
			continue
		}
		found := false
		for _, other := range chosen {
			if other.IsGTF() && matchesRef(other, want) {
				found = true
				break
			}
		}
		if !found {
			out[src.Ref()] = want
		}
	}
	return out
}

// IsGTF reports whether the source is a gene model a gene list can resolve
// through.
func (s Source) IsGTF() bool { return s.Kind == "gtf" }

// withGeneListGTF appends the GTF each chosen gene list needs, when the
// selection does not already contain it.
//
// The same shape as withDefaultReference, and for the same reason: somebody
// picking "cancer genes" is asking for the answer, not for a lesson in how the
// answer is computed. Refusing them a gene model they never mentioned would be
// technically correct and useless.
//
// Unlike a reference there is no deployment-wide default to fall back on: a gene
// list names its own GTF, so either that source is registered or the selection
// cannot work. Saying which one is missing is the whole of the error.
func (s *Store) withGeneListGTF(ctx context.Context, sourceIDs []string) ([]string, error) {
	chosen, err := s.sourcesByID(ctx, sourceIDs)
	if err != nil {
		return nil, err
	}
	missing := missingGeneListGTF(chosen)
	if len(missing) == 0 {
		return sourceIDs, nil
	}

	all, err := s.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	out := append([]string{}, sourceIDs...)
	for list, want := range missing {
		var found *Source
		for i := range all {
			if all[i].IsGTF() && matchesRef(all[i], want) {
				found = &all[i]
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%s needs the gene model %q, which is not registered; "+
				"register it, or pick a gene list whose GTF is", list, want)
		}
		out = append(out, found.ID)
	}
	return out, nil
}
