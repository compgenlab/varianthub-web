package vcfmerge

import (
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// A manifest can name an annotation anything; VCF cannot. An ID outside
// [A-Za-z_][0-9A-Za-z_.]* produces a file no parser accepts.
func TestVCFInfoIDIsAlwaysLegal(t *testing.T) {
	cases := map[string]string{
		"CADD_PHRED":    "CADD_PHRED",
		"gnomAD-AF":     "gnomAD_AF",
		"clinvar/sig":   "clinvar_sig",
		"1000G":         "_1000G", // may not start with a digit
		".leading":      "_.leading",
		"has space":     "has_space",
		"semi;colon":    "semi_colon",
		"equals=sign":   "equals_sign",
		"GENCODE_48.v2": "GENCODE_48.v2",
		"":              "_",
	}
	for in, want := range cases {
		if got := InfoID(in); got != want {
			t.Errorf("InfoID(%q) = %q, want %q", in, got, want)
		}
	}
	// Whatever the input, the result must be a legal ID.
	for in := range cases {
		got := InfoID(in)
		if got == "" {
			t.Fatalf("InfoID(%q) is empty", in)
		}
		c := got[0]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_') {
			t.Errorf("InfoID(%q) = %q starts with an illegal character", in, got)
		}
		for i := 0; i < len(got); i++ {
			b := got[i]
			ok := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
				b >= '0' && b <= '9' || b == '_' || b == '.'
			if !ok {
				t.Errorf("InfoID(%q) = %q contains %q", in, got, string(b))
			}
		}
	}
}

// A value carrying a semicolon or an equals ends the INFO field early if it is
// written literally — so the record parses, into the wrong values. That is worse
// than failing, because nothing reports it.
func TestVCFEscapeClosesTheFieldSeparators(t *testing.T) {
	got := Escape("a;b=c,d e:f%g")
	for _, bad := range []string{";", "=", ",", " ", ":"} {
		if strings.Contains(got, bad) {
			t.Errorf("escaped value still contains %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "%3B") || !strings.Contains(got, "%3D") {
		t.Errorf("expected percent-encoding, got %s", got)
	}
	// The escape character itself must be escaped first, or decoding is
	// ambiguous: a literal "%3B" would decode to a semicolon.
	if !strings.HasPrefix(Escape("%3B"), "%253B") {
		t.Errorf("a literal %%3B was not escaped: %s", Escape("%3B"))
	}
}

func TestVCFInfoValueRendering(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
		ok   bool
	}{
		{"absent", nil, "", false},
		{"empty string is absent", "", "", false},
		{"whole numbers lose the .0", float64(12), "12", true},
		{"fractions keep precision", float64(0.875), "0.875", true},
		{"true is a bare flag", true, "", true},
		{"false is absent", false, "", false},
		{"lists join with commas", []any{"a", "b"}, "a,b", true},
		{"empty list is absent", []any{}, "", false},
		{"strings are escaped", "a;b", "a%3Bb", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := InfoValue(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("InfoValue(%#v) = %q,%v; want %q,%v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Two keys that sanitise to the same ID must not merge, or one source's values
// are silently attributed to another.
func TestVCFCollidingKeysGetDistinctIDs(t *testing.T) {
	cols := []queue.Column{{Key: "a-b"}, {Key: "a/b"}, {Key: "a.b"}}
	seen := map[string]bool{}
	used := map[string]int{}
	for _, c := range cols {
		id := InfoID(c.Key)
		if n, clash := used[id]; clash {
			used[id] = n + 1
			id = id + "_" + string(rune('0'+n+1))
		} else {
			used[id] = 1
		}
		if seen[id] {
			t.Fatalf("two columns share the INFO id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("got %d distinct ids for 3 columns", len(seen))
	}
}

func TestVCFTypeMapping(t *testing.T) {
	for in, want := range map[string]string{
		"int": "Integer", "integer": "Integer",
		"float": "Float", "number": "Float",
		// The vocabulary the catalog emits, confirmed against stored job columns.
		"numeric": "Float", "text": "String", "categorical": "String",
		"bool": "Flag", "flag": "Flag",
		"string": "String", "": "String", "anything else": "String",
	} {
		if got := InfoType(in); got != want {
			t.Errorf("InfoType(%q) = %q, want %q", in, got, want)
		}
	}
}

// A quote or a newline inside a Description breaks the header line it sits in.
func TestVCFHeaderDescriptionIsQuoteSafe(t *testing.T) {
	got := HeaderDescription(queue.Column{
		Key: "k", Description: "says \"hi\"\nand more", SourceRef: "clinvar:1",
	})
	if strings.Contains(got, "\n") {
		t.Errorf("description still contains a newline: %q", got)
	}
	if strings.Contains(strings.ReplaceAll(got, `\"`, ""), `"`) {
		t.Errorf("description has an unescaped quote: %q", got)
	}
	if !strings.Contains(got, "clinvar:1") {
		t.Errorf("description lost its source attribution: %q", got)
	}
}
