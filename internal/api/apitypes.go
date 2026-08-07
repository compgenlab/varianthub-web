package api

import (
	"encoding/json"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Named response types for the published REST API.
//
// These existed only as map[string]any literals inside their handlers, which
// meant the published contract was not written down anywhere a reader — or a
// generator — could find it. A schema hand-written alongside them would be a
// second copy free to drift; a schema reflected off these cannot be, because
// they are the values the handlers actually serialize.
//
// Field names and omitempty here are the wire format, not a preference. They
// were transcribed from the responses this deployment was returning and are
// asserted against captured examples, so this is a refactor with no observable
// change.
//
// Only the published surface. The web app's own endpoints stay as they are:
// they answer to a UI shipped from this repo, which changes with them.

// PingResponse is GET /ping.
type PingResponse struct {
	Pong string `json:"pong"`
}

// BuildsResponse is GET /builds.
type BuildsResponse struct {
	Builds []catalog.Build `json:"builds"`
}

// SourceItem is one entry of GET /sources: the catalog record plus what the
// caller needs to decide whether it can be used.
type SourceItem struct {
	catalog.Source
	// Ref is "name:version", how a snapshot manifest pins it.
	Ref string `json:"ref"`
	// Annotations are the fields this source contributes, so a client can show
	// them before choosing it.
	Annotations []catalog.Annotation `json:"annotations"`
	// NeedsData is false for builtins, which compute from the variant and have
	// nothing to download.
	NeedsData bool `json:"needs_data"`
	// RequiresReference is set when the source cannot run without a reference
	// genome for its build.
	RequiresReference bool `json:"requires_reference"`
	// IsReference marks a reference genome, which contributes no annotations.
	IsReference bool `json:"is_reference"`
	// State is whether the source can actually be annotated with yet.
	State catalog.SourceState `json:"state"`
}

// SourcesResponse is GET /sources.
type SourcesResponse struct {
	Sources []SourceItem `json:"sources"`
}

// SnapshotSummary is one entry of GET /snapshots.
//
// Not a bare catalog.Snapshot: the listing carries what a client needs to
// choose one without fetching each in turn.
type SnapshotSummary struct {
	catalog.Snapshot
	SourceCount int `json:"source_count"`
	// ContainsPrivate warns that it pins something not everyone sees.
	ContainsPrivate bool `json:"contains_private"`
	// ContainsRemote warns that a run depends on somebody else's server.
	ContainsRemote bool `json:"contains_remote"`
}

// SnapshotsResponse is GET /snapshots.
type SnapshotsResponse struct {
	Snapshots []SnapshotSummary `json:"snapshots"`
}

// SnapshotResponse is GET /snapshots/{id}.
type SnapshotResponse struct {
	Snapshot catalog.Snapshot `json:"snapshot"`
	// ContainsPrivate warns that the snapshot pins something not everyone sees.
	ContainsPrivate bool `json:"contains_private"`
	// ContainsRemote warns that a run depends on somebody else's server.
	ContainsRemote bool `json:"contains_remote"`
	// Annotations are the fields this snapshot offers, each flagged with whether
	// it is on by default for this snapshot — which is a property of the
	// snapshot's selection, not of the annotation.
	Annotations []annotationOption `json:"annotations"`
}

// JobsResponse is GET /jobs.
type JobsResponse struct {
	Jobs []queue.Job `json:"jobs"`
	// Limit and Offset echo the paging that produced this page.
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	// Scoped reports that the list was narrowed to the caller's own jobs, as
	// opposed to an administrator seeing everything.
	Scoped bool `json:"scoped"`
}

// AcceptedResponse is a submission that has not finished: POST /annotate and
// POST /annotate/vcf when the caller did not wait, or waited and timed out.
type AcceptedResponse struct {
	JobID string `json:"job_id"`
	// Status is present only when the caller waited and the job was still
	// running when the wait elapsed.
	Status string `json:"status,omitempty"`
}

// JobResultResponse is a finished submission, and GET /jobs/{id}.
type JobResultResponse struct {
	JobID     string `json:"job_id"`
	Kind      string `json:"kind"`
	Snapshot  string `json:"snapshot"`
	Status    string `json:"status"`
	NVariants int64  `json:"n_variants"`
	CreatedAt int64  `json:"created_at"`
	StartedAt int64  `json:"started_at"`

	FinishedAt int64  `json:"finished_at"`
	Label      string `json:"label"`
	// Error is set only for a failed job.
	Error string `json:"error,omitempty"`
	// Results is the annotated variants, present once there are any. Raw so the
	// stored bytes are served exactly as the engine produced them.
	Results json.RawMessage `json:"results,omitempty"`
}

// CancelResponse is POST /jobs/{id}/cancel.
type CancelResponse struct {
	Job queue.Job `json:"job"`
	// Cancelled is false when the job had already finished, which is not an
	// error — the caller's intent is satisfied either way.
	Cancelled bool `json:"cancelled"`
	// Detail explains a false Cancelled.
	Detail string `json:"detail,omitempty"`
}

// ErrorResponse is any 4xx or 5xx.
type ErrorResponse struct {
	Error string `json:"error"`
	// Detail carries a longer explanation where one exists.
	Detail string `json:"detail,omitempty"`
	JobID  string `json:"job_id,omitempty"`
}
