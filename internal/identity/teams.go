package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// CreateTeam adds a team.
func (s *Store) CreateTeam(ctx context.Context, name string) (Team, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Team{}, errors.New("a team needs a name")
	}
	t := Team{ID: NewID(), Name: name, CreatedAt: s.nowFn()}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO team (id,name,created_at) VALUES ($1,$2,$3)`, t.ID, t.Name, t.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "team_name") {
			return Team{}, fmt.Errorf("%w: a team called %q", ErrExists, name)
		}
		return Team{}, err
	}
	return t, nil
}

// Team returns one team.
func (s *Store) Team(ctx context.Context, id string) (Team, error) {
	var t Team
	err := s.pool.QueryRow(ctx,
		`SELECT id,name,created_at FROM team WHERE id=$1`, id).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, fmt.Errorf("team %q: %w", id, ErrNotFound)
	}
	return t, err
}

// ListTeams returns every team.
func (s *Store) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,created_at FROM team ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTeam removes a team. Its memberships and grants go with it, so a source
// granted only to this team becomes invisible again rather than staying
// reachable through a team that no longer exists.
func (s *Store) DeleteTeam(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM team WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("team %q: %w", id, ErrNotFound)
	}
	return nil
}

// AddMember puts a user in a team.
func (s *Store) AddMember(ctx context.Context, teamID, userID, role string) error {
	if role != TeamOwner {
		role = TeamMember
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO team_member (team_id,user_id,role) VALUES ($1,$2,$3)
		ON CONFLICT (team_id,user_id) DO UPDATE SET role=excluded.role`,
		teamID, userID, role)
	return err
}

// RemoveMember takes a user out of a team.
func (s *Store) RemoveMember(ctx context.Context, teamID, userID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM team_member WHERE team_id=$1 AND user_id=$2`, teamID, userID)
	return err
}

// Member is a user's place in a team.
type Member struct {
	User User   `json:"user"`
	Role string `json:"role"`
}

// Members lists a team's people.
func (s *Store) Members(ctx context.Context, teamID string) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id,u.email,u.name,u.role,u.disabled,u.password_hash = '',
		       u.created_at,u.updated_at, m.role
		  FROM team_member m JOIN app_user u ON u.id = m.user_id
		 WHERE m.team_id=$1 ORDER BY lower(u.email)`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.User.ID, &m.User.Email, &m.User.Name, &m.User.Role,
			&m.User.Disabled, &m.User.SSO, &m.User.CreatedAt, &m.User.UpdatedAt,
			&m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TeamIDsFor returns the teams a user belongs to. Resolved once per request and
// carried on the Caller, so a visibility check is a slice scan rather than a
// query per source.
func (s *Store) TeamIDsFor(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT team_id FROM team_member WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- grants ---

// Grant lets a team see a private source.
func (s *Store) Grant(ctx context.Context, sourceID, teamID, byUserID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO source_grant (source_id,team_id,granted_by,granted_at)
		VALUES ($1,$2,$3,$4) ON CONFLICT (source_id,team_id) DO NOTHING`,
		sourceID, teamID, byUserID, s.nowFn())
	return err
}

// Revoke removes a grant.
func (s *Store) Revoke(ctx context.Context, sourceID, teamID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM source_grant WHERE source_id=$1 AND team_id=$2`, sourceID, teamID)
	return err
}

// GrantsFor lists the teams a source is granted to.
func (s *Store) GrantsFor(ctx context.Context, sourceID string) ([]Team, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id,t.name,t.created_at
		  FROM source_grant g JOIN team t ON t.id = g.team_id
		 WHERE g.source_id=$1 ORDER BY lower(t.name)`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GrantedSourceIDs returns the private sources a set of teams may see.
//
// One query for the whole request rather than one per source: a listing asks
// this once and then filters in memory.
func (s *Store) GrantedSourceIDs(ctx context.Context, teamIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(teamIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT source_id FROM source_grant WHERE team_id = ANY($1)`, teamIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
