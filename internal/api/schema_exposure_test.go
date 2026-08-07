package api

import (
	"reflect"
	"strings"
	"testing"
)

// Every exported field reachable from a published response must say, with a json
// tag, what it is called on the wire — or say `json:"-"` to stay off it.
//
// Go serializes an exported field whether or not it is tagged, so exposure is
// opt-out. The published types embed catalog and queue records, which are
// maintained for the database and the web app; a field added there for an
// internal reason would appear in the public contract under its Go name, and
// nothing would say so. This is the notice.
//
// An untagged *embedded* struct is exempt: that is how its fields are promoted
// into the parent, which is what produces the flat shape clients read.
func TestPublishedTypesTagEveryExportedField(t *testing.T) {
	var untagged []string
	seen := map[reflect.Type]bool{}

	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if t.Kind() == reflect.Map {
			walk(t.Elem(), path+"[]")
			return
		}
		if t.Kind() != reflect.Struct || seen[t] {
			return
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported is never serialized
			}
			tag, ok := f.Tag.Lookup("json")
			if !ok && !f.Anonymous {
				untagged = append(untagged, path+"."+f.Name+" ("+f.Type.String()+")")
			}
			if tag == "-" {
				continue
			}
			walk(f.Type, path+"."+f.Name)
		}
	}

	for _, v := range []any{
		PingResponse{}, BuildsResponse{}, SourcesResponse{}, SnapshotsResponse{},
		SnapshotResponse{}, JobsResponse{}, AcceptedResponse{},
		JobResultResponse{}, CancelResponse{}, ErrorResponse{},
	} {
		rt := reflect.TypeOf(v)
		walk(rt, rt.Name())
	}

	if len(untagged) > 0 {
		t.Errorf("these fields are published under their Go name with no json tag.\n"+
			"Tag each one, or mark it `json:\"-\"` to keep it off the API:\n  %s",
			strings.Join(untagged, "\n  "))
	}
}

// The manifest text is the one field explicitly kept off the wire, and it is
// reachable from the sources listing through an embedded record. Asserted
// directly because "we removed a field from the API" is the kind of decision
// that gets undone by someone adding a tag without knowing why it was absent.
func TestSourceManifestStaysOffTheAPI(t *testing.T) {
	f, ok := reflect.TypeOf(SourceItem{}).FieldByName("TOML")
	if !ok {
		return // the field is gone entirely, which is also fine
	}
	if f.Tag.Get("json") != "-" {
		t.Errorf("Source.TOML is now serialized as %q; it carries the manifest, "+
			"which is not part of the published API", f.Tag.Get("json"))
	}
}
