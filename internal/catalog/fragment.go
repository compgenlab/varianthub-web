package catalog

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// fragment is the minimum of a varhub source fragment needed to fill the
// catalog's projection columns.
//
// This does not attempt to model the whole source schema — build recipes, tool
// steps, per-chromosome templates and the rest stay opaque. toml_text remains
// the source of truth and is handed to varhub unchanged; these fields exist only
// so the catalog can be listed and filtered without re-reading every manifest.
type fragment struct {
	Sources []struct {
		Type     string `toml:"type"`
		Name     string `toml:"name"`
		Version  string `toml:"version"`
		Title    string `toml:"title"`
		Format   string `toml:"format"`
		Desc     string `toml:"description"`
		Assembly string `toml:"assembly"`
		Stream   bool   `toml:"stream"`
	} `toml:"sources"`
}

// SourceFromTOML derives a catalog Source from a fragment's text.
//
// The fragment must declare exactly one [[sources]] entry: a catalog row is one
// source, and silently taking the first of several would make the other entries
// vanish without explanation.
func SourceFromTOML(text string) (Source, error) {
	var f fragment
	if _, err := toml.Decode(text, &f); err != nil {
		return Source{}, fmt.Errorf("parse source TOML: %w", err)
	}
	switch len(f.Sources) {
	case 1:
	case 0:
		return Source{}, fmt.Errorf("no [[sources]] entry found")
	default:
		return Source{}, fmt.Errorf(
			"fragment declares %d [[sources]] entries; register one source per file",
			len(f.Sources))
	}
	s := f.Sources[0]
	if s.Name == "" || s.Version == "" {
		return Source{}, fmt.Errorf("source needs both name and version")
	}

	return Source{
		ID:      strings.ToLower(s.Name + "-" + s.Version),
		Name:    s.Name,
		Version: s.Version,
		Title:   s.Title,
		Detail:  s.Desc,
		Kind:    kindOf(s.Type, s.Format),
		// The assembly a source declares. A synthesized manifest has to state one
		// that matches, or varhub rejects the snapshot as inconsistent.
		Build: s.Assembly,
		// A streamed source is read from its url, so it has nothing to
		// provision. Projected here so the listing and the download endpoint
		// can treat it like a builtin: present and usable, with no data of its
		// own.
		Stream: s.Stream,
		TOML:   text,
	}, nil
}

// kindOf classifies a source for display. An empty type means a plain indexed
// data file, where `format` says which kind.
func kindOf(typ, format string) string {
	if typ != "" {
		return typ
	}
	if format != "" {
		return format
	}
	return "vcf" // varhub's own default for a data source with no format
}

// Annotation is one field a source can contribute, derived from its manifest.
type Annotation struct {
	Name        string `json:"name"`
	Field       string `json:"field,omitempty"` // source INFO id / column / GTF field
	Type        string `json:"type,omitempty"`  // categorical|text|numeric|flag
	Description string `json:"description,omitempty"`
	Builtin     string `json:"builtin,omitempty"` // builtin annotator name, if any
	Source      string `json:"source,omitempty"`  // populated by the caller
	SourceRef   string `json:"source_ref,omitempty"`
}

// annotationFragment is the annotation half of a source manifest.
type annotationFragment struct {
	Sources []struct {
		Annotations []struct {
			Name        string `toml:"name"`
			Field       string `toml:"field"`
			Type        string `toml:"type"`
			Description string `toml:"description"`
			Builtin     string `toml:"builtin"`
		} `toml:"annotations"`
	} `toml:"sources"`
}

// AnnotationsFromTOML lists the fields a source manifest declares.
//
// Derived on read rather than stored as a column: it is a projection of
// toml_text like the rest, and computing it here means a source registered
// before this existed needs no backfill. A manifest is a few KB, so parsing one
// per request is not worth a migration to avoid.
func AnnotationsFromTOML(text string) []Annotation {
	var f annotationFragment
	if _, err := toml.Decode(text, &f); err != nil {
		// A manifest that does not parse was rejected at registration; if one is
		// somehow stored, report no fields rather than failing the whole listing.
		return nil
	}
	var out []Annotation
	for _, s := range f.Sources {
		for _, a := range s.Annotations {
			name := a.Name
			if name == "" {
				name = a.Builtin // a builtin may name itself
			}
			if name == "" {
				continue
			}
			out = append(out, Annotation{
				Name: name, Field: a.Field, Type: a.Type,
				Description: a.Description, Builtin: a.Builtin,
			})
		}
	}
	return out
}

// Annotations returns the source's declared fields, attributed to it.
func (s Source) Annotations() []Annotation {
	anns := AnnotationsFromTOML(s.TOML)
	for i := range anns {
		anns[i].Source = s.DisplayName()
		anns[i].SourceRef = s.Ref()
	}
	return anns
}

// DisplayName is the source's title, falling back to its name.
func (s Source) DisplayName() string {
	if s.Title != "" {
		return s.Title
	}
	return s.Name
}

// streamFromTOML reports whether a manifest asks to be read from its url rather
// than downloaded. Derived on read for the same reason the annotation list is:
// it is a projection of toml_text, so nothing needs backfilling.
func streamFromTOML(text string) bool {
	var f fragment
	if _, err := toml.Decode(text, &f); err != nil || len(f.Sources) == 0 {
		return false
	}
	return f.Sources[0].Stream
}

// urlFragment is the file-location half of a source manifest.
type urlFragment struct {
	Sources []struct {
		URL      string   `toml:"url"`
		URLIndex string   `toml:"url_index"`
		Chroms   []string `toml:"chroms"`
		Alts     []string `toml:"alts"`
		Files    []struct {
			URL      string `toml:"url"`
			URLIndex string `toml:"url_index"`
		} `toml:"files"`
	} `toml:"sources"`
}

// SourceURLs lists the data URLs a source reads, with {chrom} and {alt}
// templates expanded.
//
// Index URLs are deliberately excluded: an index is small and is fetched whole
// regardless, so counting it would add noise to a figure whose point is how much
// data sits behind a network hop rather than on our own storage.
func SourceURLs(text string) []string {
	var f urlFragment
	if _, err := toml.Decode(text, &f); err != nil || len(f.Sources) == 0 {
		return nil
	}
	s := f.Sources[0]
	var raw []string
	if s.URL != "" {
		raw = append(raw, s.URL)
	}
	for _, file := range s.Files {
		if file.URL != "" {
			raw = append(raw, file.URL)
		}
	}

	alts := s.Alts
	if len(alts) == 0 {
		alts = []string{"a", "c", "g", "t"} // varhub's default per-alt set
	}
	var out []string
	for _, u := range raw {
		expanded := []string{u}
		if strings.Contains(u, "{chrom}") {
			expanded = expand(expanded, "{chrom}", s.Chroms)
		}
		if strings.Contains(u, "{alt}") {
			expanded = expand(expanded, "{alt}", alts)
		}
		out = append(out, expanded...)
	}
	return out
}

func expand(in []string, placeholder string, values []string) []string {
	var out []string
	for _, u := range in {
		for _, v := range values {
			out = append(out, strings.ReplaceAll(u, placeholder, v))
		}
	}
	return out
}
