package api

import (
	"net/http"
)

// The published REST API, as data.
//
// The router registers from this and the OpenAPI document is generated from it,
// so an endpoint cannot exist in one and not the other. Written as a table for
// exactly that reason: the spec used to be the kind of thing maintained beside
// the code and left behind by it, and this repo has watched hand-maintained
// lists drift four times already.
//
// Only what is published. Web-app routes are registered individually below the
// table, because they are not a contract anyone outside builds against.
type publishedRoute struct {
	Method  string
	Path    string
	OpID    string
	Summary string
	Handler http.HandlerFunc
	// Throttled applies the per-IP submission rate.
	Throttled bool
	// Public skips the credential check, leaving authorization entirely to the
	// handler's own check on the object.
	//
	// For reads of a single job by id. An anonymous result's link IS its
	// credential, so requiring a second one defeats the sharing it exists for:
	// a link works in a browser only because the app hands every visitor a
	// session on page load, and the same link handed to curl was refused before
	// anyone asked whose job it was.
	//
	// Safe because the handler still decides. An unauthenticated caller reading
	// a signed-in user's job gets the same 404 it always did — canView says no,
	// and nothing about skipping requireAuth changes that. Enumeration is not a
	// concern at 128 random bits.
	Public bool
	// Request is a zero value of the request body type, or nil for a body-less
	// endpoint. RequestSchema overrides it where reflection cannot express the
	// shape.
	Request       any
	RequestSchema *schema
	// Response is a zero value of the success response type.
	Response any
	// Status is the success status, when it is not 200.
	Status int
	// Produces overrides the response media type for endpoints that do not
	// return JSON.
	Produces string
	Params   []param
}

// param is a path or query parameter.
type param struct {
	Name     string
	In       string // "path" | "query"
	Doc      string
	Required bool
	Type     string // "string" | "integer" | "boolean"
	Enum     []string
}

func (s *Server) publishedRoutes() []publishedRoute {
	return []publishedRoute{
		{
			Method: "GET", Path: "/api/v1/ping", OpID: "ping",
			Summary:  "Check that a credential works.",
			Handler:  s.handlePing,
			Response: PingResponse{},
		},
		{
			Method: "GET", Path: "/api/v1/builds", OpID: "listBuilds",
			Summary:  "List the genome assemblies this installation offers.",
			Handler:  s.handleListBuilds,
			Response: BuildsResponse{},
		},
		{
			Method: "GET", Path: "/api/v1/sources", OpID: "listSources",
			Summary:  "List annotation sources, one row per name and version.",
			Handler:  s.handleSources,
			Response: SourcesResponse{},
		},
		{
			Method: "GET", Path: "/api/v1/snapshots", OpID: "listSnapshots",
			Summary:  "List published snapshots.",
			Handler:  s.handleSnapshots,
			Response: SnapshotsResponse{},
		},
		{
			Method: "GET", Path: "/api/v1/snapshots/{id}", OpID: "getSnapshot",
			Summary:  "Fetch one snapshot and the exact source versions it pins.",
			Handler:  s.handleSnapshot,
			Response: SnapshotResponse{},
			Params: []param{{
				Name: "id", In: "path", Required: true, Type: "string",
				Doc: "The snapshot's identifier.",
			}},
		},
		{
			Method: "POST", Path: "/api/v1/annotate", OpID: "annotate",
			Summary:       "Submit variants for annotation.",
			Handler:       s.handleAnnotate,
			Throttled:     true,
			Request:       annotateRequest{},
			RequestSchema: annotateRequestSchema(),
			Response:      AcceptedResponse{},
			Status:        http.StatusAccepted,
		},
		{
			Method: "POST", Path: "/api/v1/annotate/vcf", OpID: "annotateVCF",
			Summary: "Submit a VCF file for annotation.",
			Handler: s.handleAnnotateVCF, Throttled: true,
			Response: AcceptedResponse{},
			Status:   http.StatusAccepted,
		},
		{
			Method: "GET", Path: "/api/v1/jobs", OpID: "listJobs",
			Summary:  "List your jobs.",
			Handler:  s.handleListJobs,
			Response: JobsResponse{},
			Params: []param{
				{Name: "limit", In: "query", Type: "integer", Doc: "Page size."},
				{Name: "offset", In: "query", Type: "integer", Doc: "Rows to skip."},
			},
		},
		{
			Method: "GET", Path: "/api/v1/jobs/{id}", OpID: "getJob",
			Public: true,
			Summary: "Fetch a job's status and its chunks. Results are a " +
				"separate call, GET /jobs/{id}/export.",
			Handler:  s.handleGetJob,
			Response: JobResponse{},
			Params: []param{{
				Name: "id", In: "path", Required: true, Type: "string",
				Doc: "The job identifier returned by a submission.",
			}},
		},
		{
			Method: "POST", Path: "/api/v1/jobs/{id}/cancel", OpID: "cancelJob",
			Summary:  "Stop a queued or running job.",
			Handler:  s.handleCancelJob,
			Response: CancelResponse{},
			Params: []param{{
				Name: "id", In: "path", Required: true, Type: "string",
				Doc: "The job identifier.",
			}},
		},
		{
			Method: "GET", Path: "/api/v1/jobs/{id}/export", OpID: "exportResults",
			Public:  true,
			Summary: "Download a finished job's results, whole, in a chosen format. " +
				"format=vcf may answer 302 with a short-lived link straight to " +
				"object storage, so follow redirects.",
			Handler: s.handleExport,
			// Streamed in four formats, so the body is not one JSON schema.
			Produces: "application/json, text/tab-separated-values, text/csv, application/gzip",
			Params: []param{
				{
					Name: "id", In: "path", Required: true, Type: "string",
					Doc: "The job identifier.",
				},
				{
					Name: "format", In: "query", Type: "string",
					Enum: []string{"json", "tsv", "csv", "vcf"},
					Doc: "Defaults to json, or to vcf for a job submitted as a VCF. " +
						"vcf is available for any job: a locus list yields a VCF with " +
						"ID, QUAL and FILTER missing and the annotations as INFO. " +
						"vcf comes back gzipped, as variants-<id>.vcf.gz — it is the " +
						"stored object served as it is. Where object storage is " +
						"reachable by the caller this answers 302 with a presigned " +
						"URL good for 15 minutes, so the transfer never passes " +
						"through the API; otherwise the same bytes are relayed. The " +
						"other three formats are converted from that same file and " +
						"always stream from here.",
				},
				{
					Name: "q", In: "query", Type: "string",
					Doc: "Case-insensitive substring filter across the locus and annotation values.",
				},
				{
					Name: "sort", In: "query", Type: "string",
					Doc: "\"locus\", or an annotation field name. VCF output is always in " +
						"coordinate order regardless, since any other order cannot be indexed.",
				},
				{
					Name: "order", In: "query", Type: "string",
					Enum: []string{"asc", "desc"}, Doc: "Sort direction.",
				},
			},
		},
	}
}

// register wires the published table into the mux.
func (s *Server) registerPublished(mux *http.ServeMux) {
	for _, rt := range s.publishedRoutes() {
		var h http.Handler = rt.Handler
		if rt.Throttled {
			h = s.throttle(h)
		}
		if !rt.Public {
			h = s.requireAuth(h)
		}
		mux.Handle(rt.Method+" "+rt.Path, h)
	}
}
