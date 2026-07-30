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
		Type    string `toml:"type"`
		Name    string `toml:"name"`
		Version string `toml:"version"`
		Title   string `toml:"title"`
		Format  string `toml:"format"`
		Desc    string `toml:"description"`
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
		TOML:    text,
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
