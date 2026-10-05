package db

import (
	"context"
	"errors"
	"iter"
	"slices"

	"go.kenn.io/kata/internal/uid"
)

// ErrTeamNameExists reports a conflicting hub-local team name.
var ErrTeamNameExists = errors.New("team name already exists")

// ErrProjectAccessRevisionConflict reports a stale project policy revision.
var ErrProjectAccessRevisionConflict = errors.New("project access revision conflict")

// Team groups canonical actors on one daemon. Names do not identify teams
// across daemons, and teammate attribution does not grant membership.
type Team struct {
	UID      string `json:"uid"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

// ProjectAccessPolicy intersects an authenticated principal's other grants.
// A teams policy with no teams denies ordinary access.
type ProjectAccessPolicy struct {
	ProjectUID string   `json:"project_uid"`
	Visibility string   `json:"visibility"`
	TeamUIDs   []string `json:"team_uids"`
	Revision   int64    `json:"revision"`
}

// ProjectAccessStorage is the native project-policy contract. Administrative
// mutations append daemon-local audit events and return the exact committed event.
type ProjectAccessStorage interface {
	CreateTeam(ctx context.Context, name, adminActor string) (Team, Event, error)
	TeamByUID(ctx context.Context, teamUID string) (Team, error)
	ListTeams(ctx context.Context) ([]Team, error)
	DeleteTeam(ctx context.Context, teamUID, adminActor string) (Event, error)
	TeamMembers(ctx context.Context, teamUID string) ([]string, error)
	SetTeamMembership(ctx context.Context, teamUID, actor string, present bool, adminActor string) (Event, error)
	MigrateTeamActor(ctx context.Context, fromActor, toActor, adminActor string) (Event, error)
	ProjectAccessPolicy(ctx context.Context, projectUID string) (ProjectAccessPolicy, error)
	SetProjectAccessPolicy(ctx context.Context, policy ProjectAccessPolicy, adminActor string) (ProjectAccessPolicy, Event, error)
	AccessibleProjectUIDs(ctx context.Context, actor string) ([]string, error)
	AnonymousAccessibleProjectUIDs(ctx context.Context) ([]string, error)
	ProjectAccessRevision(ctx context.Context) (int64, error)
	ProjectAccessTransactionFence(actor string, projectUIDs []string) TransactionFence
	ExportProjectAccess(ctx context.Context) iter.Seq2[ImportRecord, error]
}

// TeamMembership is a hub-local membership retained by owner backups.
type TeamMembership struct {
	TeamUID string `json:"team_uid"`
	Actor   string `json:"actor"`
}

// ImportKind identifies this record for JSONL import and export.
func (*Team) ImportKind() string { return "team" }

// ImportKind identifies this record for JSONL import and export.
func (*TeamMembership) ImportKind() string { return "team_membership" }

// ImportKind identifies this record for JSONL import and export.
func (*ProjectAccessPolicy) ImportKind() string { return "project_access_policy" }

// NormalizeInitialTeams validates the complete enrollment before any writes.
func NormalizeInitialTeams(teamUIDs []string) ([]string, error) {
	result := slices.Clone(teamUIDs)
	slices.Sort(result)
	for i, teamUID := range result {
		if !uid.Valid(teamUID) || (i > 0 && result[i-1] == teamUID) {
			return nil, errors.New("invalid or duplicate team UID")
		}
	}
	return result, nil
}
