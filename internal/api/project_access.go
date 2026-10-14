package api

import "go.kenn.io/kata/internal/db"

// CreateTeamRequest creates a team under the daemon owner’s administrative authority.
type CreateTeamRequest struct {
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"256"`
	}
}

// TeamRequest selects a team by immutable UID.
type TeamRequest struct {
	TeamUID string `path:"team_uid"`
}

// TeamMembershipRequest selects the canonical actor membership in one team.
type TeamMembershipRequest struct {
	TeamUID string `path:"team_uid"`
	Actor   string `path:"actor"`
}

// TeamResponse returns a team and its committed administrative event.
type TeamResponse struct {
	Body struct {
		Team  db.Team   `json:"team"`
		Event *db.Event `json:"event,omitempty"`
	}
}

// ListTeamsResponse returns the owner-authorized team inventory.
type ListTeamsResponse struct {
	Body struct {
		Teams []db.Team `json:"teams"`
	}
}

// ShowTeamResponse returns a team and its canonical actor members.
type ShowTeamResponse struct {
	Body struct {
		Team    db.Team  `json:"team"`
		Members []string `json:"members"`
	}
}

// AccessAdministrationResponse reports a committed membership or team change.
type AccessAdministrationResponse struct {
	Body struct {
		Changed bool      `json:"changed"`
		Event   *db.Event `json:"event,omitempty"`
	}
}

// ProjectAccessRequest selects one project’s visibility policy.
type ProjectAccessRequest struct {
	ProjectID int64 `path:"project_id"`
}

// SetProjectAccessRequest sets visibility and team grants against the observed revision.
type SetProjectAccessRequest struct {
	ProjectID int64 `path:"project_id"`
	Body      struct {
		Visibility string   `json:"visibility" enum:"all,teams"`
		TeamUIDs   []string `json:"team_uids" maxItems:"256"`
		Revision   int64    `json:"revision,omitempty" minimum:"0"`
	}
}

// ProjectAccessResponse returns the selected visibility policy and committed change event.
type ProjectAccessResponse struct {
	Body struct {
		Policy db.ProjectAccessPolicy `json:"policy"`
		Event  *db.Event              `json:"event,omitempty"`
	}
}
