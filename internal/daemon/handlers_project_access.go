package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerProjectAccessHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "createTeam", Method: http.MethodPost, Path: "/api/v1/teams"}, func(ctx context.Context, in *api.CreateTeamRequest) (*api.TeamResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(in.Body.Name)
		if name == "" || strings.ContainsAny(name, "\r\n\x00") {
			return nil, api.NewError(400, "validation", "invalid team name", "", nil)
		}
		team, event, err := cfg.DB.CreateTeam(ctx, name, tokenAdminAuditActor(ctx, db.BootstrapActor))
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		cfg.Publish().Event(event.ProjectID, event)
		out := &api.TeamResponse{}
		out.Body.Team = team
		out.Body.Event = &event
		return out, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "listTeams", Method: http.MethodGet, Path: "/api/v1/teams"}, func(ctx context.Context, _ *struct{}) (*api.ListTeamsResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		teams, err := cfg.DB.ListTeams(ctx)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.ListTeamsResponse{}
		out.Body.Teams = teams
		return out, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "showTeam", Method: http.MethodGet, Path: "/api/v1/teams/{team_uid}"}, func(ctx context.Context, in *api.TeamRequest) (*api.ShowTeamResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		team, err := cfg.DB.TeamByUID(ctx, in.TeamUID)
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		members, err := cfg.DB.TeamMembers(ctx, in.TeamUID)
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		out := &api.ShowTeamResponse{}
		out.Body.Team = team
		out.Body.Members = members
		return out, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "deleteTeam", Method: http.MethodDelete, Path: "/api/v1/teams/{team_uid}"}, func(ctx context.Context, in *api.TeamRequest) (*api.AccessAdministrationResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		event, err := cfg.DB.DeleteTeam(ctx, in.TeamUID, tokenAdminAuditActor(ctx, db.BootstrapActor))
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		return accessAdministrationResult(cfg, event), nil
	})
	for _, action := range []struct {
		method, id string
		present    bool
	}{{http.MethodPut, "addTeamMember", true}, {http.MethodDelete, "removeTeamMember", false}} {
		huma.Register(humaAPI, huma.Operation{OperationID: action.id, Method: action.method, Path: "/api/v1/teams/{team_uid}/members/{actor}"}, func(ctx context.Context, in *api.TeamMembershipRequest) (*api.AccessAdministrationResponse, error) {
			if err := ensureTokenAdminAllowed(ctx); err != nil {
				return nil, err
			}
			if err := db.ValidateTokenActor(in.Actor); err != nil {
				return nil, api.NewError(400, "validation", err.Error(), "", nil)
			}
			event, err := cfg.DB.SetTeamMembership(ctx, in.TeamUID, in.Actor, action.present, tokenAdminAuditActor(ctx, db.BootstrapActor))
			if err != nil {
				return nil, projectAccessAPIError(err)
			}
			return accessAdministrationResult(cfg, event), nil
		})
	}
	huma.Register(humaAPI, huma.Operation{OperationID: "getProjectAccess", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/access"}, func(ctx context.Context, in *api.ProjectAccessRequest) (*api.ProjectAccessResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		policy, err := cfg.DB.ProjectAccessPolicy(ctx, project.UID)
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		out := &api.ProjectAccessResponse{}
		out.Body.Policy = policy
		return out, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "setProjectAccess", Method: http.MethodPut, Path: "/api/v1/projects/{project_id}/access"}, func(ctx context.Context, in *api.SetProjectAccessRequest) (*api.ProjectAccessResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		teams, err := db.NormalizeInitialTeams(in.Body.TeamUIDs)
		if err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		if in.Body.Visibility == "all" && len(teams) > 0 {
			return nil, api.NewError(400, "validation", "all visibility cannot contain teams", "", nil)
		}
		if err := validateInitialTeamAccess(ctx, cfg.DB, teams); err != nil {
			return nil, err
		}
		policy, event, err := cfg.DB.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: in.Body.Visibility, TeamUIDs: teams, Revision: in.Body.Revision}, tokenAdminAuditActor(ctx, db.BootstrapActor))
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		cfg.Publish().Event(event.ProjectID, event)
		out := &api.ProjectAccessResponse{}
		out.Body.Policy = policy
		out.Body.Event = &event
		return out, nil
	})
}

func accessAdministrationResult(cfg ServerConfig, event db.Event) *api.AccessAdministrationResponse {
	out := &api.AccessAdministrationResponse{}
	out.Body.Changed = event.ID != 0
	if event.ID != 0 {
		cfg.Publish().Event(event.ProjectID, event)
		out.Body.Event = &event
	}
	return out
}

func projectAccessAPIError(err error) error {
	if errors.Is(err, db.ErrTeamNameExists) {
		return api.NewError(http.StatusConflict, "team_exists", "team name already exists", "", nil)
	}
	if errors.Is(err, db.ErrProjectAccessRevisionConflict) {
		return api.NewError(http.StatusConflict, "revision_conflict", "project access policy changed", "reload the current policy before editing", nil)
	}
	if errors.Is(err, db.ErrNotFound) {
		return projectAccessDenied()
	}
	return internalAPIError(err)
}

func validateInitialTeamAccess(ctx context.Context, store db.Storage, teams []string) error {
	for _, teamUID := range teams {
		if _, err := store.TeamByUID(ctx, teamUID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return api.NewError(400, "invalid_team", "team does not exist", "", nil)
			}
			return internalAPIError(err)
		}
	}
	return nil
}
