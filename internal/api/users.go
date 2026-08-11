package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/identity"
)

// Accounts, teams, and grants. Every route here is behind requireAdmin except
// the first-administrator path, which is what requireAdmin is bootstrapped by.

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.identity.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

type userRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if req.Role == "" {
		req.Role = identity.RoleMember
	}
	u, err := s.identity.CreateUser(r.Context(), req.Email, req.Name, req.Role, req.Password)
	if err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}

	// Creating the first administrator is what the bootstrap credential exists
	// for, so it is spent here rather than lingering as a second way in.
	if c := callerOf(r); c.Bootstrap && u.IsAdmin() {
		if tok := bearerOf(r); tok != "" {
			if err := s.identity.ConsumeBootstrap(r.Context(), tok); err != nil {
				// The account exists either way, and an administrator now does
				// too — which is itself enough to stop the bootstrap working.
				writeJSON(w, http.StatusCreated, map[string]any{"user": u})
				return
			}
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": u})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role     *string `json:"role"`
		Tier     *string `json:"tier"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	id := r.PathValue("id")
	if req.Role != nil {
		if err := s.identity.SetRole(r.Context(), id, *req.Role); err != nil {
			writeError(w, statusForIdentity(err), err.Error())
			return
		}
	}
	if req.Tier != nil {
		// Checked here rather than in the identity store: which tiers exist is a
		// question about this deployment's settings, and that package has no
		// business knowing the answer. Rejected rather than coerced, because a
		// tier the server does not recognize resolves to the standard limits —
		// so silently accepting a typo would look like a promotion that worked.
		if !catalog.ValidTier(*req.Tier) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("unknown tier %q (want one of %s)",
					*req.Tier, strings.Join(catalog.Tiers, ", ")))
			return
		}
		if err := s.identity.SetTier(r.Context(), id, *req.Tier); err != nil {
			writeError(w, statusForIdentity(err), err.Error())
			return
		}
	}
	if req.Disabled != nil {
		if err := s.identity.SetDisabled(r.Context(), id, *req.Disabled); err != nil {
			writeError(w, statusForIdentity(err), err.Error())
			return
		}
	}
	if req.Password != nil {
		if err := s.identity.SetPassword(r.Context(), id, *req.Password); err != nil {
			writeError(w, statusForIdentity(err), err.Error())
			return
		}
	}
	u, err := s.identity.User(r.Context(), id)
	if err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// --- teams ---

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.identity.ListTeams(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		identity.Team
		Members []identity.Member `json:"members"`
	}
	out := make([]item, 0, len(teams))
	for _, t := range teams {
		members, err := s.identity.Members(r.Context(), t.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, item{Team: t, Members: members})
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": out})
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	t, err := s.identity.CreateTeam(r.Context(), req.Name)
	if err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"team": t})
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	err := s.identity.DeleteTeam(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if err := s.identity.AddMember(r.Context(), r.PathValue("id"), req.UserID, req.Role); err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	err := s.identity.RemoveMember(r.Context(), r.PathValue("id"), r.PathValue("user"))
	if err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- grants on private sources ---

func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	teams, err := s.identity.GrantsFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}

func (s *Server) handleGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TeamID string `json:"team_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	err := s.identity.Grant(r.Context(), r.PathValue("id"), req.TeamID, callerOf(r).UserID())
	if err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	err := s.identity.Revoke(r.Context(), r.PathValue("id"), r.PathValue("team"))
	if err != nil {
		writeError(w, statusForIdentity(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// statusForIdentity maps a store error to a status. Anything unrecognised is a
// 400 rather than a 500: the store's own validation failures are the caller's
// fault, and reporting them as server errors hides real ones.
func statusForIdentity(err error) int {
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, identity.ErrExists):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}
