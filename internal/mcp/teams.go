package mcpserver

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/pkg/client/generated"
)

// TeamsInput requests the hub-local team catalog.
type TeamsInput struct{}

// TeamsOutput returns the authorized team catalog.
type TeamsOutput struct {
	Teams []generated.Team `json:"teams"`
}

// TeamCreateInput supplies a new hub-local team name.
type TeamCreateInput struct {
	Name string `json:"name"`
}

// TeamInput selects a team by immutable UID.
type TeamInput struct {
	TeamUID string `json:"team_uid"`
}

// TeamOutput returns a team and its canonical actor members.
type TeamOutput struct {
	Team    generated.Team `json:"team"`
	Members []string       `json:"members,omitempty"`
}

// TeamMemberSetInput adds or removes one canonical actor’s membership.
type TeamMemberSetInput struct {
	TeamUID     string `json:"team_uid"`
	MemberActor string `json:"member_actor"`
	Present     bool   `json:"present"`
}

// TeamChangeOutput reports whether a team mutation changed retained state.
type TeamChangeOutput struct {
	Changed bool `json:"changed"`
}

// ProjectAccessShowInput selects a project whose visibility policy will be read.
type ProjectAccessShowInput struct {
	Project string `json:"project"`
}

// ProjectAccessSetInput supplies visibility, team UIDs and an optional expected policy revision.
type ProjectAccessSetInput struct {
	Project    string   `json:"project"`
	Visibility string   `json:"visibility"`
	TeamUIDs   []string `json:"team_uids,omitempty"`
	Revision   *int64   `json:"revision,omitempty"`
}

// ProjectAccessOutput returns the selected project’s visibility policy.
type ProjectAccessOutput struct {
	Policy generated.ProjectAccessPolicy `json:"policy"`
}

func registerTeamTools(server *sdkmcp.Server, h toolHandlers) {
	read := toolHints(true, false, false)
	write := toolHints(false, true, false)
	addTool(server, "kata.teams", "List teams", "List teams through the selected daemon's owner authority.", read, h.teams)
	addTool(server, "kata.team_create", "Create team", "Create a project-access team.", nonIdempotent(toolHints(false, false, false)), h.teamCreate)
	addTool(server, "kata.team_show", "Show team", "Read one team and its canonical actor members by UID.", read, h.teamShow)
	addTool(server, "kata.team_delete", "Delete team", "Delete a team without broadening restricted projects to all visibility.", write, h.teamDelete)
	addTool(server, "kata.team_member_set", "Set team membership", "Add or remove the canonical actor's membership by team UID.", write, h.teamMemberSet)
	addTool(server, "kata.project_access_show", "Show project access", "Read project visibility and team UIDs.", read, h.projectAccessShow)
	addTool(server, "kata.project_access_set", "Set project access", "Set all or teams visibility. A stale revision conflicts; omitting revision captures the current policy before writing.", write, h.projectAccessSet)
}

func (h toolHandlers) teams(ctx context.Context, _ *sdkmcp.CallToolRequest, _ TeamsInput) (*sdkmcp.CallToolResult, TeamsOutput, error) {
	if err := h.requireDaemonWideScope("team administration"); err != nil {
		return nil, TeamsOutput{}, err
	}
	response, err := h.options.Client.ListTeams(ctx)
	if err != nil {
		return nil, TeamsOutput{}, err
	}
	return successResult(), TeamsOutput{Teams: response.Teams}, nil
}
func (h toolHandlers) teamCreate(ctx context.Context, _ *sdkmcp.CallToolRequest, in TeamCreateInput) (*sdkmcp.CallToolResult, TeamOutput, error) {
	if err := h.requireDaemonWideScope("team administration"); err != nil {
		return nil, TeamOutput{}, err
	}
	response, err := h.options.Client.CreateTeam(ctx, &generated.CreateTeamRequestOptions{Body: &generated.CreateTeamBody{Name: in.Name}})
	if err != nil {
		return nil, TeamOutput{}, err
	}
	return successResult(), TeamOutput{Team: response.Team}, nil
}
func (h toolHandlers) teamShow(ctx context.Context, _ *sdkmcp.CallToolRequest, in TeamInput) (*sdkmcp.CallToolResult, TeamOutput, error) {
	if err := h.requireDaemonWideScope("team administration"); err != nil {
		return nil, TeamOutput{}, err
	}
	response, err := h.options.Client.ShowTeam(ctx, &generated.ShowTeamRequestOptions{PathParams: &generated.ShowTeamPath{TeamUID: in.TeamUID}})
	if err != nil {
		return nil, TeamOutput{}, err
	}
	return successResult(), TeamOutput{Team: response.Team, Members: response.Members}, nil
}
func (h toolHandlers) teamDelete(ctx context.Context, _ *sdkmcp.CallToolRequest, in TeamInput) (*sdkmcp.CallToolResult, TeamChangeOutput, error) {
	if err := h.requireDaemonWideScope("team administration"); err != nil {
		return nil, TeamChangeOutput{}, err
	}
	response, err := h.options.Client.DeleteTeam(ctx, &generated.DeleteTeamRequestOptions{PathParams: &generated.DeleteTeamPath{TeamUID: in.TeamUID}})
	if err != nil {
		return nil, TeamChangeOutput{}, err
	}
	return successResult(), TeamChangeOutput{Changed: response.Changed}, nil
}
func (h toolHandlers) teamMemberSet(ctx context.Context, _ *sdkmcp.CallToolRequest, in TeamMemberSetInput) (*sdkmcp.CallToolResult, TeamChangeOutput, error) {
	if err := h.requireDaemonWideScope("team administration"); err != nil {
		return nil, TeamChangeOutput{}, err
	}
	if in.Present {
		response, err := h.options.Client.AddTeamMember(ctx, &generated.AddTeamMemberRequestOptions{PathParams: &generated.AddTeamMemberPath{TeamUID: in.TeamUID, Actor: in.MemberActor}})
		if err != nil {
			return nil, TeamChangeOutput{}, err
		}
		return successResult(), TeamChangeOutput{Changed: response.Changed}, nil
	}
	response, err := h.options.Client.RemoveTeamMember(ctx, &generated.RemoveTeamMemberRequestOptions{PathParams: &generated.RemoveTeamMemberPath{TeamUID: in.TeamUID, Actor: in.MemberActor}})
	if err != nil {
		return nil, TeamChangeOutput{}, err
	}
	return successResult(), TeamChangeOutput{Changed: response.Changed}, nil
}
func (h toolHandlers) projectAccessShow(ctx context.Context, _ *sdkmcp.CallToolRequest, in ProjectAccessShowInput) (*sdkmcp.CallToolResult, ProjectAccessOutput, error) {
	if err := h.requireDaemonWideScope("team administration"); err != nil {
		return nil, ProjectAccessOutput{}, err
	}
	project, err := h.options.Scope.Project(ctx, h.options.Client, in.Project, false)
	if err != nil {
		return nil, ProjectAccessOutput{}, err
	}
	response, err := h.options.Client.GetProjectAccess(ctx, &generated.GetProjectAccessRequestOptions{PathParams: &generated.GetProjectAccessPath{ProjectID: project.ID}})
	if err != nil {
		return nil, ProjectAccessOutput{}, err
	}
	return successResult(), ProjectAccessOutput{Policy: response.Policy}, nil
}
func (h toolHandlers) projectAccessSet(ctx context.Context, _ *sdkmcp.CallToolRequest, in ProjectAccessSetInput) (*sdkmcp.CallToolResult, ProjectAccessOutput, error) {
	if err := h.requireDaemonWideScope("team administration"); err != nil {
		return nil, ProjectAccessOutput{}, err
	}
	project, err := h.options.Scope.Project(ctx, h.options.Client, in.Project, false)
	if err != nil {
		return nil, ProjectAccessOutput{}, err
	}
	revision := in.Revision
	if revision == nil {
		current, err := h.options.Client.GetProjectAccess(ctx, &generated.GetProjectAccessRequestOptions{PathParams: &generated.GetProjectAccessPath{ProjectID: project.ID}})
		if err != nil {
			return nil, ProjectAccessOutput{}, err
		}
		revision = &current.Policy.Revision
	}
	response, err := h.options.Client.SetProjectAccess(ctx, &generated.SetProjectAccessRequestOptions{PathParams: &generated.SetProjectAccessPath{ProjectID: project.ID}, Body: &generated.SetProjectAccessBody{Visibility: generated.SetProjectAccessRequestBodyVisibility(in.Visibility), TeamUids: append([]string{}, in.TeamUIDs...), Revision: revision}})
	if err != nil {
		return nil, ProjectAccessOutput{}, err
	}
	return successResult(), ProjectAccessOutput{Policy: response.Policy}, nil
}
