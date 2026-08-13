// Package vcfmerge writes a submitted VCF back with annotations added to it.
//
// Here rather than in the API because three different things do it, and only
// one of them is a download. A VCF job's answer is its own file annotated; a
// chunk of a split VCF is merged by the worker that ran it; and the collect
// step joins those chunks into one file. Leaving it in the request handler
// would have meant the worker could not reach it — and the two would have
// drifted, which for a merge means the same submission coming back with
// different INFO ids depending on which path produced it.
//
// The rendered-from-rows VCF next door in the API is a fine answer for a locus
// list, which never had a file. It is a poor one here: it returns a skeleton
// carrying only the columns this server knows about, so a submitted ID, QUAL,
// FILTER, INFO, FORMAT and every sample column are dropped. Someone who sent a
// two-sample tumour/normal VCF got back two bare loci.
//
// cghts does the parsing and writing: an unmodified record round-trips verbatim
// and a modified one is rebuilt from its parsed model, which is what makes
// everything this server does not care about survive untouched.
package vcfmerge

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// InfoID renders an annotation key as a VCF INFO ID.
//
// The spec requires an ID matching [A-Za-z_][0-9A-Za-z_.]*, and annotation keys
// are manifest-supplied, so a key with a hyphen or a slash would otherwise write
// a file no parser accepts. Substitution is per character and deterministic so
// two runs of the same snapshot produce the same IDs.
func InfoID(key string) string {
	var b strings.Builder
	for i, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9', r == '.':
			// Legal, but not as the first character.
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// InfoType maps a column's declared type onto a VCF INFO type.
//
// Anything unrecognised becomes String, which every value can be written as.
// Guessing Integer for a column that turns out to hold "." or a range would
// produce a file that a strict parser rejects.
func InfoType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "int", "integer":
		return "Integer"
	// "numeric" is the vocabulary the catalog actually emits — checked against
	// the columns of finished jobs, not guessed. Without it every score
	// declared itself a String, which parses but stops a consumer treating it
	// as a number.
	case "numeric", "float", "number", "double":
		return "Float"
	case "bool", "boolean", "flag":
		return "Flag"
	default:
		// text, categorical, and anything a future manifest invents. Every
		// value can be written as a String; guessing Integer for a column that
		// turns out to hold "." would produce a file a strict parser rejects.
		return "String"
	}
}

// HeaderDescription renders a column's prose for a header line.
func HeaderDescription(c queue.Column) string {
	d := c.Description
	if d == "" {
		d = c.Label
	}
	if d == "" {
		d = c.Key
	}
	if c.SourceRef != "" {
		d += " [" + c.SourceRef + "]"
	}
	// Inside a quoted header value, so quotes and backslashes are what break it.
	d = strings.ReplaceAll(d, `\`, `\\`)
	d = strings.ReplaceAll(d, `"`, `\"`)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, d)
}

// InfoValue renders one annotation value, or "" when there is nothing to write.
func InfoValue(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case bool:
		// A Flag is present or absent; there is no "=false".
		return "", t
	case string:
		if t == "" {
			return "", false
		}
		return Escape(t), true
	case float64:
		// JSON numbers arrive as float64. Render a whole number without the
		// trailing ".0" a naive format would add, so an integer column reads as
		// one.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), true
		}
		return strconv.FormatFloat(t, 'g', -1, 64), true
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := InfoValue(e); ok && s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, ","), true
	default:
		s := Escape(fmt.Sprint(t))
		if s == "" {
			return "", false
		}
		return s, true
	}
}

// uniqueInfoID is the INFO id an annotation is written under, avoiding a
// collision with one the submitter already uses.
//
// Overwriting theirs would silently replace data they sent with data we
// computed, and under a name they chose — the kind of loss that is invisible
// until somebody compares the file with what they submitted.
func uniqueInfoID(key string, taken map[string]bool) string {
	id := InfoID(key)
	if !taken[id] {
		taken[id] = true
		return id
	}
	for n := 2; ; n++ {
		alt := fmt.Sprintf("%s_%d", id, n)
		if !taken[alt] {
			taken[alt] = true
			return alt
		}
	}
}

// VariantKey identifies a record's allele for looking up its annotations.
func VariantKey(chrom string, pos int64, ref, alt string) string {
	return fmt.Sprintf("%s:%d:%s:%s", chrom, pos, ref, alt)
}

// annotateRecord sets a record's annotation INFO fields from the results.
//
// A multi-allelic record is annotated per ALT and the values joined with
// commas, in ALT order, which is how VCF expresses a per-allele field. Writing
// only the first allele's value would attribute it to all of them.
func annotateRecord(rec *vcf.VcfRecord, byAllele map[string]map[string]any,
	cols []queue.Column, ids map[string]string, flag map[string]bool) {

	alts := rec.Alt()
	if len(alts) == 0 {
		return
	}
	perAlt := make([]map[string]any, 0, len(alts))
	for _, alt := range alts {
		perAlt = append(perAlt, byAllele[VariantKey(rec.Chrom, int64(rec.Pos), rec.Ref, alt)])
	}

	info := rec.Info()
	changed := false
	// Column order, not map order. Ranging the id map would put a record's
	// INFO fields in a different sequence on every run, so the same job would
	// export bytes that never match twice.
	for _, c := range cols {
		key := c.Key
		id := ids[key]
		vals := make([]string, len(perAlt))
		any := false
		for i, anns := range perAlt {
			if anns == nil {
				continue
			}
			v, ok := InfoValue(anns[key])
			if !ok {
				continue
			}
			any = true
			if flag[key] {
				vals[i] = ""
				continue
			}
			vals[i] = v
		}
		if !any {
			continue
		}
		if flag[key] {
			info.SetFlag(id)
			changed = true
			continue
		}
		// Missing values within a multi-allelic field are "." rather than
		// empty, or the comma-separated list would not line up with the ALTs.
		for i, v := range vals {
			if v == "" {
				vals[i] = "."
			}
		}
		info.Set(id, strings.Join(vals, ","))
		changed = true
	}
	// Attributes has no back-reference to its record, so a record is written
	// verbatim unless it is told it changed. Without this the annotations were
	// set and then discarded on the way out — the file came back byte-perfect
	// and useless.
	if changed {
		rec.MarkDirty()
	}
}

// Escape percent-encodes the characters INFO cannot carry literally.
//
// Without this a description or a value containing a semicolon or an equals
// silently ends the field early, producing a file that parses into the wrong
// values rather than failing — the worst kind of wrong.
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%':
			b.WriteString("%25")
		case ':':
			b.WriteString("%3A")
		case ';':
			b.WriteString("%3B")
		case '=':
			b.WriteString("%3D")
		case ',':
			b.WriteString("%2C")
		case '\r':
			b.WriteString("%0D")
		case '\n':
			b.WriteString("%0A")
		case '\t':
			b.WriteString("%09")
		case ' ':
			b.WriteString("%20")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
