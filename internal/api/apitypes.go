package api

import (
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
	GeneListGTF       string               `json:"genelist_gtf,omitempty" doc:"For a gene-list source, the gene model it resolves variants through, as \"name\" or \"name:version\". Selecting the list pins this too — it cannot answer without one."`
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

// AcceptedResponse is a queued submission.
//
// Every submission answers this way, whatever happens next. Annotation is
// asynchronous, so a submission's whole result is the identifier to follow it
// by — and three calls then each do one thing: GET /jobs/{id} for status,
// /jobs/{id}/export for results, /jobs/{id}/cancel to stop it. The alternative
// is one response shape that means different things depending on how quickly
// the work happened to finish, which every client has to handle both ways.
type AcceptedResponse struct {
	JobID string `json:"job_id" doc:"Poll GET /jobs/{id} for progress, and GET /jobs/{id}/export for results."`
}

// JobStatusResponse is GET /jobs/{id}: what a job is and how far it has got.
//
// Its own type rather than queue.Job, which is the database row. The row also
// carries client_ip, session and user_id, and this route is deliberately public
// — an anonymous result's link is its credential, so it is meant to be passed
// to someone else. Returning the row would hand whoever holds a shared link the
// submitter's address and session identifier, which is not what sharing a result
// is understood to share.
//
// Results are not here and never were. They are GET /jobs/{id}/export, which
// streams and takes a format; embedding them would make a status poll download
// the whole result set every few seconds.
type JobStatusResponse struct {
	JobID      string `json:"job_id" doc:"Stable identifier for this run."`
	Kind       string `json:"kind" doc:"\"locus\" for a variant list, \"vcf\" for a submitted file."`
	Snapshot   string `json:"snapshot" doc:"The snapshot annotated against. An individual-source selection becomes a generated snapshot, which is what makes the run reproducible."`
	Selection  string `json:"selection" doc:"The annotation fields asked for, or empty for the snapshot's defaults."`
	Status     string `json:"status" doc:"queued | running | done | error | cancelled. Fetch results once this is \"done\"."`
	NVariants  int64  `json:"n_variants" doc:"How many variants were annotated."`
	CreatedAt  int64  `json:"created_at" doc:"Unix seconds."`
	StartedAt  int64  `json:"started_at,omitempty" doc:"Unix seconds. Absent until a worker claims it."`
	FinishedAt int64  `json:"finished_at,omitempty" doc:"Unix seconds. Absent until it finishes."`
	Label      string `json:"label,omitempty" doc:"A short human label: the locus, or the submitted filename."`
	Error      string `json:"error,omitempty" doc:"Why the job failed, when it did."`
}

// jobStatus projects a queue row onto the published shape.
func jobStatus(j queue.Job) JobStatusResponse {
	return JobStatusResponse{
		JobID: j.ID, Kind: j.Kind, Snapshot: j.Snapshot, Selection: j.Selection,
		Status: j.Status, NVariants: j.NVariants, CreatedAt: j.CreatedAt,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Label: j.Label,
		Error: j.Error,
	}
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
