package enginetest

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// Do --format json and --format vcf produce the same annotations?
//
// This is the question PR 2 rests on. They are different code in varhub — the
// engine for json, the streaming pipeline for vcf — and the swap is only safe if
// the values they attach to a variant are the same values. A disagreement here
// is an annotation landing on the wrong variant, which is the one failure that
// looks exactly like success.
//
// The fixture is chosen for the cases that break naive agreement rather than the
// ones that confirm it: a key that is not a legal INFO id, a multi-allelic
// record where the two alleles differ, an indel, a value carrying a separator,
// and a locus no source matches.
func TestTheJSONAndVCFPathsAgree(t *testing.T) {
	h := Build(t, Fixture{
		Annotations: []Annotation{
			{Name: "sig", Field: "SIG"},
			// A numeric field, read from a source field of a different name.
			// It used to be "gnomAD-AF" here, to see what varhub did with a name
			// that is not a legal INFO id — the answer was that it wrote the
			// hyphen straight into the header. varhub refuses such a name at
			// manifest-validation time now, so the case cannot be built; see
			// TestVarhubRefusesAnIllegalINFOKey.
			{Name: "gnomad_af", Field: "AF", Type: "numeric"},
		},
		Records: []Record{
			{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G",
				Info: map[string]string{"SIG": "pathogenic", "AF": "0.125"}},
			// Two alleles at one position with different values: the case where
			// a per-record field silently gives both the same answer.
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "A",
				Info: map[string]string{"SIG": "benign", "AF": "0.5"}},
			{Chrom: "chr1", Pos: 500, Ref: "G", Alt: "C",
				Info: map[string]string{"SIG": "uncertain", "AF": "0.25"}},
			// An indel, and a value carrying a separator. The fixture escapes it on the way
			// in, as a valid VCF must, and varhub hands the escaped text back
			// unchanged on both paths — which is itself worth pinning.
			{Chrom: "chr2", Pos: 1500, Ref: "GG", Alt: "G",
				Info: map[string]string{"SIG": "risk;factor"}},
		},
	})

	loci := []string{
		"chr1:100:A:G",
		"chr1:500:G:A",
		"chr1:500:G:C",
		"chr2:1500:GG:G",
		"chr3:999:T:C", // nothing matches this one
	}

	fromJSON := annotateJSON(t, h, loci)
	fromVCF := annotateVCF(t, h, loci)

	if len(fromJSON) != len(loci) {
		t.Fatalf("the json path returned %d loci, want %d: %v", len(fromJSON), len(loci), fromJSON)
	}
	for _, l := range loci {
		j, v := fromJSON[l], fromVCF[l]
		if len(j) == 0 && len(v) == 0 {
			continue
		}
		for key, jv := range j {
			vv, ok := v[key]
			if !ok {
				t.Errorf("%s: the vcf path lost %q (json had %q)", l, key, jv)
				continue
			}
			if jv != vv {
				t.Errorf("%s %q: json=%q vcf=%q", l, key, jv, vv)
			}
		}
		for key, vv := range v {
			if _, ok := j[key]; !ok {
				t.Errorf("%s: the vcf path invented %q=%q", l, key, vv)
			}
		}
	}
}

// annotateJSON returns locus -> key -> value, as text, from the engine path.
func annotateJSON(t *testing.T, h Home, loci []string) map[string]map[string]string {
	t.Helper()
	args := append([]string{"-home", h.Dir, "annotate", "--format", "json"}, loci...)
	out, err := exec.Command(h.Bin, args...).Output()
	if err != nil {
		t.Fatalf("annotate --format json: %v", err)
	}
	var rows []struct {
		Chrom       string         `json:"chrom"`
		Pos         int64          `json:"pos"`
		Ref         string         `json:"ref"`
		Alt         string         `json:"alt"`
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("parse json output: %v\n%s", err, out)
	}
	got := map[string]map[string]string{}
	for _, r := range rows {
		key := locusKey(r.Chrom, r.Pos, r.Ref, r.Alt)
		vals := map[string]string{}
		for k, v := range r.Annotations {
			if s := scalar(v); s != "" {
				vals[k] = s
			}
		}
		got[key] = vals
	}
	return got
}

// annotateVCF returns the same shape from the streaming pipeline.
func annotateVCF(t *testing.T, h Home, loci []string) map[string]map[string]string {
	t.Helper()
	args := append([]string{"-home", h.Dir, "annotate", "--format", "vcf"}, loci...)
	out, err := exec.Command(h.Bin, args...).Output()
	if err != nil {
		t.Fatalf("annotate --format vcf: %v", err)
	}
	got := map[string]map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 8 {
			continue
		}
		vals := map[string]string{}
		if f[7] != "." {
			for _, kv := range strings.Split(f[7], ";") {
				k, v, has := strings.Cut(kv, "=")
				if !has {
					vals[k] = "true" // a bare flag
					continue
				}
				// Raw, not decoded. varhub passes a source's percent-escaping
				// through untouched on BOTH paths — the json output carries
				// "risk%3Bfactor" exactly as the VCF does — so decoding one side
				// here would compare two different representations and report a
				// disagreement that is the test's own.
				vals[k] = v
			}
		}
		// One record per allele here, since the input was a locus list.
		got[locusKey(f[0], atoi(t, f[1]), f[3], f[4])] = vals
	}
	return got
}

func locusKey(chrom string, pos int64, ref, alt string) string {
	return strings.Join([]string{chrom, itoa(pos), ref, alt}, ":")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a position: %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// scalar renders a JSON annotation value the way the VCF path would write it,
// so the two are comparable as text. Absent, null and empty all read as absent.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return ""
	case string:
		return t
	case float64:
		return trimFloat(t)
	}
	return ""
}

func trimFloat(f float64) string {
	s := strings.TrimRight(strings.TrimRight(formatFloat(f), "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// varhub refuses an annotation name that cannot be a VCF INFO key.
//
// Pinned here because this service depends on it. A name is written into the
// output as an INFO id verbatim, so this guarantee is what makes a stored result
// VCF parseable by a strict reader — and it is a guarantee made in another
// repository, which is exactly the kind that goes quietly missing.
func TestVarhubRefusesAnIllegalINFOKey(t *testing.T) {
	bin := Binary(t)
	dir := t.TempDir()
	WriteIllegalFixture(t, dir)

	out, err := exec.Command(bin, "-home", dir, "annotation", "list", "--format", "json",
		"--", "testsnap").CombinedOutput()
	if err == nil {
		t.Fatalf("a manifest declaring \"gnomAD-AF\" was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), "INFO key") {
		t.Errorf("the refusal does not say what is wrong:\n%s", out)
	}
}

// A numeric annotation is declared Float in the VCF header.
//
// This service reads types out of a stored result VCF to decide whether a value
// is a number or text — see vcfmerge.Rows — so a score declared String comes back
// as the string "0.125" and the json export stops emitting numbers. varhub used
// to declare everything String because the annotator API had nowhere to put a
// type (cghts#40, varianthub-cli#27); this pins the guarantee now that it does.
func TestANumericAnnotationIsDeclaredFloat(t *testing.T) {
	h := Build(t, Fixture{
		Annotations: []Annotation{
			{Name: "sig", Field: "SIG", Type: "categorical"},
			{Name: "gnomad_af", Field: "AF", Type: "numeric"},
		},
		Records: []Record{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G",
			Info: map[string]string{"SIG": "pathogenic", "AF": "0.125"}}},
	})

	out, err := exec.Command(h.Bin, "-home", h.Dir, "annotate", "--format", "vcf",
		"chr1:100:A:G").Output()
	if err != nil {
		t.Fatalf("annotate --format vcf: %v", err)
	}
	got := string(out)
	// Number=A, not 1: an exact match writes one value per ALT, so that is what
	// the header has to say. See TestAMultiAllelicRecordKeepsItsAllelesStraight.
	if !strings.Contains(got, "ID=gnomad_af,Number=A,Type=Float") {
		t.Errorf("a numeric annotation was not declared Float:\n%s", got)
	}
	// And a categorical one is still a String, so the type is being read from
	// the manifest rather than applied to everything.
	if !strings.Contains(got, "ID=sig,Number=A,Type=String") {
		t.Errorf("a categorical annotation was not declared String:\n%s", got)
	}
}
