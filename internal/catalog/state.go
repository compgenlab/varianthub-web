package catalog

import (
	"context"
	"strings"
)

// Provisioning states a source can be in.
const (
	StateInstalling = "installing"
	StateReady      = "ready"
	StateFailed     = "failed"
)

// SourceState is whether a source can be annotated with yet.
type SourceState struct {
	State     string `json:"state" doc:"installing | ready | failed. Annotating with a source that is not ready fails."`
	Error     string `json:"error,omitempty" doc:"Why provisioning failed, when it did."`
	UpdatedAt int64  `json:"updated_at,omitempty" doc:"Unix seconds."`
	Job       string `json:"job,omitempty" doc:"The download currently working on it, when one is."`
}

// Ready reports whether annotation with this source would work.
func (s SourceState) Ready() bool { return s.State == StateReady }

// SetSourceState records where provisioning got to.
func (s *Store) SetSourceState(ctx context.Context, sourceID, state, errMsg string) error {
	// Errors can be long; the column is for telling someone what went wrong,
	// not for holding a transcript. The job's log has the full text.
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000] + "…"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO source_state (source_id, state, error, updated_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (source_id) DO UPDATE
		   SET state = excluded.state, error = excluded.error,
		       updated_at = excluded.updated_at`,
		sourceID, state, errMsg, s.nowFn())
	return err
}

// SetSourceStates records the same outcome for several sources, which is what a
// download job produces: one run, one result, however many sources it covered.
func (s *Store) SetSourceStates(ctx context.Context, sourceIDs []string, state, errMsg string) error {
	for _, id := range sourceIDs {
		if id = strings.TrimSpace(id); id != "" {
			if err := s.SetSourceState(ctx, id, state, errMsg); err != nil {
				return err
			}
		}
	}
	return nil
}

// SourceStates returns the recorded state of every source that has one.
func (s *Store) SourceStates(ctx context.Context) (map[string]SourceState, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source_id, state, error, updated_at FROM source_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SourceState{}
	for rows.Next() {
		var id string
		var st SourceState
		if err := rows.Scan(&id, &st.State, &st.Error, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}
