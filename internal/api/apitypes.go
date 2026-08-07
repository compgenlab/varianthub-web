package api

import (
	"encoding/json"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Named response types for the published REST API.
//
// Field prose lives in `doc` tags rather than in comments beside the fields.
// The tags are what the OpenAPI schema is built from, so a comment saying the
// same thing would be a second copy free to drift from the published one.
//
// Field names and omitempty are the wire format, not a preference: they were
// transcribed from responses captured from a running deployment and are pinned
// by TestPublishedResponseKeys.
//
// Only the published surface. The web app's own endpoints keep their maps —
// they answer to a UI shipped from this repo, which changes with them.

// PingResponse is GET /ping.
type PingResponse struct {
	Pong string `json:"pong" doc:"Always \"ok\". A reachability check that costs the server nothing."`
}

// BuildsResponse is GET /builds.
type BuildsResponse struct {
	Builds []catalog.Build `json:"builds" doc:"Genome assemblies this installation offers, in picker order."`
}

// SourceItem is one entry of GET /sources.
type SourceItem struct {
	catalog.Source
	Ref               string               `json:"ref" doc:"\"name:version\" — how a snapshot manifest pins this source."`
	Annotations       []catalog.Annotation `json:"annotations" doc:"The fields this source contributes, so a client can see them before choosing it."`
	NeedsData         bool                 `json:"needs_data" doc:"False for builtins, which compute from the variant itself and have nothing to download."`
	RequiresReference bool                 `json:"requires_reference" doc:"The source cannot run without a reference genome for its build. VEP declares this."`
	IsReference       bool                 `json:"is_reference" doc:"A reference genome. Contributes no annotations and is chosen separately."`
	State             catalog.SourceState  `json:"state" doc:"Whether the source can be annotated with yet. Registering one and being able to use it are different things: a tool needs its image and setup, and until then every annotation using it fails."`
}

// SourcesResponse is GET /sources.
type SourcesResponse struct {
	Sources []SourceItem `json:"sources" doc:"One row per (name, version). Private sources the caller has no grant for are absent entirely."`
}

// SnapshotSummary is one entry of GET /snapshots.
type SnapshotSummary struct {
	catalog.Snapshot
	SourceCount     int  `json:"source_count" doc:"How many sources the snapshot pins."`
	ContainsPrivate bool `json:"contains_private" doc:"The snapshot pins something not every caller can see."`
	ContainsRemote  bool `json:"contains_remote" doc:"The snapshot pins a source read over the network at query time, so a run depends on somebody else's server being up."`
}

// SnapshotsResponse is GET /snapshots.
type SnapshotsResponse struct {
	Snapshots []SnapshotSummary `json:"snapshots" doc:"Published snapshots. Drafts are omitted."`
}

// SnapshotResponse is GET /snapshots/{id}.
type SnapshotResponse struct {
	Snapshot        catalog.Snapshot   `json:"snapshot" doc:"The snapshot and the exact source versions it pins."`
	ContainsPrivate bool               `json:"contains_private" doc:"The snapshot pins something not every caller can see."`
	ContainsRemote  bool               `json:"contains_remote" doc:"The snapshot pins a source read over the network at query time."`
	Annotations     []annotationOption `json:"annotations" doc:"The fields this snapshot offers, each flagged with whether it is on by default — which is a property of the snapshot's selection, not of the annotation."`
}

// JobsResponse is GET /jobs.
type JobsResponse struct {
	Jobs   []queue.Job `json:"jobs" doc:"Jobs, newest first."`
	Limit  int         `json:"limit" doc:"The page size that produced this list."`
	Offset int         `json:"offset" doc:"The offset that produced this list."`
	Scoped bool        `json:"scoped" doc:"The list was narrowed to the caller's own jobs, as opposed to an administrator seeing everything."`
}

// AcceptedResponse is a submission that has not finished.
type AcceptedResponse struct {
	JobID  string `json:"job_id" doc:"Poll GET /jobs/{id} for progress, and GET /jobs/{id}/export for results."`
	Status string `json:"status,omitempty" doc:"Present only when the caller waited and the job was still running when the wait elapsed."`
}

// JobResultResponse is a finished submission, and GET /jobs/{id}.
type JobResultResponse struct {
	JobID      string          `json:"job_id" doc:"Stable identifier for this run."`
	Kind       string          `json:"kind" doc:"\"locus\" for a variant list, \"vcf\" for a submitted file."`
	Snapshot   string          `json:"snapshot" doc:"The snapshot annotated against. An individual-source selection becomes a generated snapshot, which is what makes the run reproducible."`
	Status     string          `json:"status" doc:"queued | running | done | error | cancelled."`
	NVariants  int64           `json:"n_variants" doc:"How many variants were annotated."`
	CreatedAt  int64           `json:"created_at" doc:"Unix seconds."`
	StartedAt  int64           `json:"started_at" doc:"Unix seconds. Zero until a worker claims it."`
	FinishedAt int64           `json:"finished_at" doc:"Unix seconds. Zero until it finishes."`
	Label      string          `json:"label" doc:"A human-readable summary of what was submitted."`
	Error      string          `json:"error,omitempty" doc:"Set only for a failed job."`
	Results    json.RawMessage `json:"results,omitempty" doc:"The annotated variants, present once there are any. For large result sets prefer GET /jobs/{id}/export, which streams."`
}

// CancelResponse is POST /jobs/{id}/cancel.
type CancelResponse struct {
	Job       queue.Job `json:"job" doc:"The job as it stands after the request."`
	Cancelled bool      `json:"cancelled" doc:"False when the job had already finished, which is not an error — the caller's intent is satisfied either way."`
	Detail    string    `json:"detail,omitempty" doc:"Explains a false cancelled."`
}

// ErrorResponse is any 4xx or 5xx.
type ErrorResponse struct {
	Error  string `json:"error" doc:"What went wrong, in a sentence."`
	Detail string `json:"detail,omitempty" doc:"A longer explanation where one exists."`
	JobID  string `json:"job_id,omitempty" doc:"The job the error concerns, where it concerns one."`
}
