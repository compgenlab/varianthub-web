package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
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
		{"snapshot", SnapshotResponse{}, []string{
			"annotations", "contains_private", "contains_remote", "snapshot"}},
		{"jobs", JobsResponse{}, []string{"jobs", "limit", "offset", "scoped"}},
		{"accepted", AcceptedResponse{}, []string{"job_id"}},
		{"cancel", CancelResponse{}, []string{"cancelled", "job"}},
		{"error", ErrorResponse{}, []string{"error"}},
		{"job result", JobResultResponse{}, []string{
			"created_at", "finished_at", "job_id", "kind", "label",
			"n_variants", "snapshot", "started_at", "status"}},
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
