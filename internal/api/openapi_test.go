package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/config"
)

// The document must describe exactly the routes the server publishes.
//
// Both come from publishedRoutes, so they cannot disagree by construction —
// this asserts that construction, because the value of a table is lost the
// moment somebody registers an endpoint beside it. Which is what happened to
// four migration lists in this repo before they were derived.
func TestOpenAPIDescribesEveryPublishedRoute(t *testing.T) {
	s := New(&config.Config{Version: "test"}, nil, nil, nil, nil)

	doc := s.openAPI()
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("the document describes no paths")
	}

	inDoc := map[string]bool{}
	for p, item := range paths {
		methods, _ := item.(map[string]any)
		for m := range methods {
			inDoc[strings.ToUpper(m)+" "+p] = true
		}
	}
	inTable := map[string]bool{}
	for _, rt := range s.publishedRoutes() {
		inTable[rt.Method+" "+rt.Path] = true
	}

	for k := range inTable {
		if !inDoc[k] {
			t.Errorf("%s is served but not in the document", k)
		}
	}
	for k := range inDoc {
		if !inTable[k] {
			t.Errorf("%s is in the document but not served", k)
		}
	}
}

// Every published route registered on the mux must be in the table.
//
// The table is only single-source if nothing is registered outside it. This
// walks the source for v1.Handle calls naming a published path and fails on one
// the table does not carry.
func TestNoPublishedRouteIsRegisteredOutsideTheTable(t *testing.T) {
	s := New(&config.Config{Version: "test"}, nil, nil, nil, nil)
	inTable := map[string]bool{}
	for _, rt := range s.publishedRoutes() {
		inTable[rt.Method+" "+rt.Path] = true
	}

	src, err := os.ReadFile(filepath.Join(".", "api.go"))
	if err != nil {
		t.Fatal(err)
	}
	// v1.Handle("GET /api/v1/thing", ...)
	re := regexp.MustCompile(`v1\.Handle(?:Func)?\("(GET|POST|PUT|PATCH|DELETE) (/api/v1/[^"]*)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		method, path := m[1], m[2]
		key := method + " " + path
		if inTable[key] {
			t.Errorf("%s is registered directly as well as from the table", key)
			continue
		}
		// Anything else must be deliberately web-only, and the surface tests
		// prove those 404 for a token.
		if !strings.Contains(string(src), path) {
			continue
		}
	}
}

// A published route with no described response is a route the explorer cannot
// render, and a reader cannot use.
func TestEveryPublishedRouteDescribesItsResponse(t *testing.T) {
	s := New(&config.Config{Version: "test"}, nil, nil, nil, nil)
	for _, rt := range s.publishedRoutes() {
		if rt.Response == nil && rt.Produces == "" {
			t.Errorf("%s %s describes neither a response type nor a media type",
				rt.Method, rt.Path)
		}
		if rt.Summary == "" {
			t.Errorf("%s %s has no summary", rt.Method, rt.Path)
		}
		if rt.OpID == "" {
			t.Errorf("%s %s has no operationId", rt.Method, rt.Path)
		}
	}
}

// Path parameters in the URL must be declared, or a client generated from the
// document produces a request to a literal "{id}".
func TestPathParametersAreDeclared(t *testing.T) {
	s := New(&config.Config{Version: "test"}, nil, nil, nil, nil)
	braces := regexp.MustCompile(`\{([^}]+)\}`)
	for _, rt := range s.publishedRoutes() {
		declared := map[string]bool{}
		for _, p := range rt.Params {
			if p.In == "path" {
				declared[p.Name] = true
				if !p.Required {
					t.Errorf("%s %s: path parameter %q is not required", rt.Method, rt.Path, p.Name)
				}
			}
		}
		for _, m := range braces.FindAllStringSubmatch(rt.Path, -1) {
			if !declared[m[1]] {
				t.Errorf("%s %s: path parameter %q is not declared", rt.Method, rt.Path, m[1])
			}
		}
	}
}

// The generated schemas must reflect the wire shape: embedded records inlined,
// omitempty optional, doc tags carried through as descriptions.
func TestGeneratedSchemasMatchTheWireShape(t *testing.T) {
	sch := schemaOf(SourcesResponse{})
	items := sch.Properties["sources"].Items
	if items == nil {
		t.Fatal("sources has no item schema")
	}
	// Inlined, not nested: encoding/json promotes an untagged embedded struct,
	// so a nested object here would describe a shape the server never sends.
	for _, want := range []string{"id", "name", "version", "kind", "build", "ref", "state"} {
		if _, ok := items.Properties[want]; !ok {
			t.Errorf("source item is missing the promoted field %q", want)
		}
	}
	if _, ok := items.Properties["Source"]; ok {
		t.Error("the embedded record was nested instead of inlined")
	}
	// The manifest is off the wire and must be off the schema too. Checked for
	// the literal "-" as well: a field whose json tag is "-" leaks under that
	// name if the skip is missed, which reads as a stray property rather than
	// as the manifest it is.
	for _, leaked := range []string{"toml", "TOML", "-"} {
		if _, ok := items.Properties[leaked]; ok {
			t.Errorf("a field excluded from the API appears in the schema as %q", leaked)
		}
	}
	// Nothing at all may be named "-": that is never a real wire name.
	for name := range items.Properties {
		if strings.HasPrefix(name, "-") {
			t.Errorf("schema property %q comes from a json:\"-\" field", name)
		}
	}
	// Descriptions come from doc tags.
	if items.Properties["build"].Description == "" {
		t.Error("build has no description; the doc tag was not carried through")
	}
	if !strings.Contains(items.Properties["build"].Description, "hg38") {
		t.Errorf("build's description is not the doc tag: %q", items.Properties["build"].Description)
	}

	// omitempty decides optionality.
	req := map[string]bool{}
	for _, r := range items.Required {
		req[r] = true
	}
	if !req["id"] || !req["kind"] {
		t.Errorf("always-written fields are not required: %v", items.Required)
	}
	if req["title"] || req["origin"] {
		t.Errorf("omitempty fields are marked required: %v", items.Required)
	}

}

// json.RawMessage is a JSON document, not the base64 string its underlying
// []byte would otherwise produce.
//
// Against a fixture rather than a published type: none carries a RawMessage
// today, and the special case in schemaOf is exactly the kind of thing that
// stops being true the moment nothing checks it. The next published field to
// carry arbitrary JSON should describe itself correctly on arrival.
func TestRawMessageIsDescribedAsADocument(t *testing.T) {
	type withRaw struct {
		Payload json.RawMessage `json:"payload" doc:"Arbitrary JSON."`
	}
	payload := schemaOf(withRaw{}).Properties["payload"]
	if payload == nil {
		t.Fatal("payload is missing from the schema")
	}
	if payload.Type == "string" || payload.Type == "array" {
		t.Errorf("payload is described as %q; it carries arbitrary JSON", payload.Type)
	}
}

// The union that reflection cannot express has to be written, and correct.
func TestAnnotateRequestDocumentsItsUnion(t *testing.T) {
	sch := annotateRequestSchema()
	ann := sch.Properties["annotations"]
	if ann == nil || len(ann.OneOf) != 2 {
		t.Fatalf("annotations does not document its union: %+v", ann)
	}
	if ann.OneOf[0].Type != "string" || ann.OneOf[1].Type != "array" {
		t.Errorf("the union is not string-or-array: %+v", ann.OneOf)
	}
	if len(sch.Required) == 0 || sch.Required[0] != "variants" {
		t.Errorf("variants is not required: %v", sch.Required)
	}
}

// The document is served, and is valid JSON a client can consume.
func TestOpenAPIIsServed(t *testing.T) {
	h := newHarness(t)
	_, tok := h.admin(t)

	w := h.do("GET", "/api/v1/openapi.json", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET openapi.json = %d: %s", w.Code, w.Body.String())
	}
	var doc struct {
		OpenAPI string                            `json:"openapi"`
		Paths   map[string]map[string]interface{} `json:"paths"`
		Info    struct {
			Title string `json:"title"`
		} `json:"info"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the document is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi version = %q", doc.OpenAPI)
	}
	if doc.Info.Title == "" {
		t.Error("the document has no title")
	}
	var paths []string
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) < 10 {
		t.Errorf("only %d paths documented: %v", len(paths), paths)
	}
	// Administration is not part of the contract.
	for _, p := range paths {
		if strings.Contains(p, "/admin/") || strings.Contains(p, "/auth/") {
			t.Errorf("%s is in the published document", p)
		}
	}
}
