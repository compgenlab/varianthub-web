package api

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// VCF output for any job, rendered from the stored rows.
//
// Available whatever the input was, not only for a submitted VCF. A locus list
// annotated here is still a set of variants, and a VCF is what the next tool in
// somebody's pipeline reads — so the fields a locus list cannot supply are
// written as missing rather than withheld as a reason not to offer the format.
// ID, QUAL and FILTER are "." and there are no samples; CHROM, POS, REF and ALT
// are real, and the annotations are the INFO.
//
// A job submitted as a VCF gets this too. Preserving the submitted file's own
// headers, INFO and sample columns is a separate and larger thing — it needs the
// input kept and the annotations merged back onto it — and this does not pretend
// to be that.

// vcfInfoID renders an annotation key as a VCF INFO ID.
//
// The spec requires an ID matching [A-Za-z_][0-9A-Za-z_.]*, and annotation keys
// are manifest-supplied, so a key with a hyphen or a slash would otherwise write
// a file no parser accepts. Substitution is per character and deterministic so
// two runs of the same snapshot produce the same IDs.
func vcfInfoID(key string) string {
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

// vcfEscape percent-encodes the characters INFO cannot carry literally.
//
// Without this a description or a value containing a semicolon or an equals
// silently ends the field early, producing a file that parses into the wrong
// values rather than failing — the worst kind of wrong.
func vcfEscape(s string) string {
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

// vcfType maps a column's declared type onto a VCF INFO type.
//
// Anything unrecognised becomes String, which every value can be written as.
// Guessing Integer for a column that turns out to hold "." or a range would
// produce a file that a strict parser rejects.
func vcfType(t string) string {
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

// vcfHeaderDescription renders a column's prose for a header line.
func vcfHeaderDescription(c queue.Column) string {
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

// vcfInfoValue renders one annotation value, or "" when there is nothing to write.
func vcfInfoValue(v any) (string, bool) {
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
		return vcfEscape(t), true
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
			if s, ok := vcfInfoValue(e); ok && s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, ","), true
	default:
		s := vcfEscape(fmt.Sprint(t))
		if s == "" {
			return "", false
		}
		return s, true
	}
}

// exportVCF streams the job's results as a VCF.
func (s *Server) exportVCF(w http.ResponseWriter, r *http.Request, job queue.Job,
	cols []queue.Column, qy queue.ResultQuery) {

	// Coordinate order, whatever was asked for. A VCF sorted by CADD is not a
	// VCF anything can index, and it would look perfectly fine until someone ran
	// tabix on it. The search filter is still honoured — that changes which
	// records appear, not their order.
	qy.Sort, qy.Desc = "locus", false

	flag := map[string]bool{}
	ids := make(map[string]string, len(cols))
	used := map[string]int{}
	for _, c := range cols {
		id := vcfInfoID(c.Key)
		// Two keys can sanitise to the same ID, and silently merging them would
		// attribute one source's values to another.
		if n, clash := used[id]; clash {
			used[id] = n + 1
			id = fmt.Sprintf("%s_%d", id, n+1)
		} else {
			used[id] = 1
		}
		ids[c.Key] = id
		flag[c.Key] = vcfType(c.Type) == "Flag"
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, "##fileformat=VCFv4.2\n")
	fmt.Fprintf(w, "##source=VariantHub %s\n", vcfEscape(s.cfg.Version))
	fmt.Fprintf(w, "##varianthub_job=%s\n", vcfEscape(job.ID))
	if job.Snapshot != "" {
		fmt.Fprintf(w, "##varianthub_snapshot=%s\n", vcfEscape(job.Snapshot))
	}
	for _, c := range cols {
		number := "1"
		typ := vcfType(c.Type)
		if typ == "Flag" {
			number = "0"
		}
		fmt.Fprintf(w, "##INFO=<ID=%s,Number=%s,Type=%s,Description=\"%s\">\n",
			ids[c.Key], number, typ, vcfHeaderDescription(c))
	}
	fmt.Fprint(w, "#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n")

	err := s.queue.StreamResults(r.Context(), job.ID, qy, func(v queue.Variant) error {
		// Sorted so a record's INFO does not depend on Go's map iteration order,
		// which would make two identical runs differ byte for byte.
		keys := make([]string, 0, len(v.Annotations))
		for k := range v.Annotations {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		info := make([]string, 0, len(keys))
		for _, k := range keys {
			id, known := ids[k]
			if !known {
				id = vcfInfoID(k)
			}
			val, ok := vcfInfoValue(v.Annotations[k])
			if !ok {
				continue
			}
			if val == "" || flag[k] {
				info = append(info, id) // a Flag is its own presence
				continue
			}
			info = append(info, id+"="+val)
		}
		field := strings.Join(info, ";")
		if field == "" {
			field = "." // an empty INFO column is not valid; missing is "."
		}
		alt := v.Alt
		if alt == "" {
			alt = "."
		}
		ref := v.Ref
		if ref == "" {
			ref = "."
		}
		_, wErr := fmt.Fprintf(w, "%s\t%d\t.\t%s\t%s\t.\t.\t%s\n",
			v.Chrom, v.Pos, ref, alt, field)
		return wErr
	})
	if err != nil {
		// The header is already on the wire, so this cannot become a clean
		// error response. Log it; the client sees a short file.
		log.Printf("api: vcf export %s: %v", job.ID, err)
	}
}
