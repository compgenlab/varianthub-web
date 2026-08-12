package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// The published responses' JSON keys, pinned.
//
// These types replaced map[string]any literals inside their handlers, and the
// replacement had to be invisible on the wire — the React app and any existing
// client read these names. Verified once against responses captured from a
// running deployment; kept here so a later rename is caught by the suite rather
// than by somebody's broken client.
//
// A key set rather than a golden document: values change with the data, names
// are the contract.
func TestPublishedResponseKeys(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want []string
	}{
		{"ping", PingResponse{}, []string{"pong"}},
		{"builds", BuildsResponse{}, []string{"builds"}},
		{"sources", SourcesResponse{}, []string{"sources"}},
		{"snapshots", SnapshotsResponse{}, []string{"snapshots"}},
		// `visibility` is the level the snapshot is offered at, derived from the
		// sources it pins. `constrained_by` is omitempty — absent on a public
		// snapshot, which is why it is not listed here.
		{"snapshot", SnapshotResponse{}, []string{
			"annotations", "contains_private", "contains_remote", "snapshot", "visibility"}},
		{"jobs", JobsResponse{}, []string{"jobs", "limit", "offset", "scoped"}},
		{"accepted", AcceptedResponse{}, []string{"job_id"}},
		{"cancel", CancelResponse{}, []string{"cancelled", "job"}},
		{"error", ErrorResponse{}, []string{"error"}},
		{"job status", JobStatusResponse{}, []string{
			"created_at", "job_id", "kind", "n_variants", "selection",
			"snapshot", "status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(m))
			for k := range m {
				got = append(got, k)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("keys = %v, want %v", got, tc.want)
			}
		})
	}
}

// The two listing entries embed a catalog record, so their keys are the record's
// plus the additions. Asserted on the additions, because those are the fields
// this package owns and can rename by accident.
func TestListingItemsCarryTheirAdditions(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  any
		want []string
	}{
		{"source item", SourceItem{}, []string{
			"annotations", "is_reference", "needs_data", "ref", "requires_reference", "state"}},
		{"snapshot summary", SnapshotSummary{}, []string{
			"contains_private", "contains_remote", "source_count"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			for _, k := range tc.want {
				if _, ok := m[k]; !ok {
					t.Errorf("%s lost the key %q", tc.name, k)
				}
			}
		})
	}
}

// GET /jobs/{id} is deliberately public — an anonymous result's link is its
// credential, so it is meant to be handed to someone else. The queue row it is
// built from carries the submitter's address, session and account, and sharing a
// result is not understood to share any of those.
//
// Asserted on the projection rather than on a live response so it fails at the
// moment someone widens the type, which is when it is cheap to notice.
func TestJobStatusDoesNotCarryTheSubmittersIdentity(t *testing.T) {
	full := queue.Job{
		ID: "j1", Kind: "locus", Snapshot: "snap", Selection: "af",
		Status: "done", NVariants: 3, Label: "chr1:100:A:T",
		ClientIP: "203.0.113.7", Session: "sess-secret", UserID: "user-42",
		CreatedAt: 1, StartedAt: 2, FinishedAt: 3,
	}
	b, err := json.Marshal(jobStatus(full))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, leaked := range []string{"203.0.113.7", "sess-secret", "user-42"} {
		if strings.Contains(body, leaked) {
			t.Errorf("a shared job link discloses %q:\n%s", leaked, body)
		}
	}
	// And it still says the things a caller polls it for.
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"job_id", "status", "n_variants", "finished_at"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%q is missing from a job status", want)
		}
	}
}
