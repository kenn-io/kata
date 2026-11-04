package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// CronAccessPolicyPrefix is reserved for daemon-local, nonreplicated authority.
const CronAccessPolicyPrefix = "cron_access_policy."

// ErrCronAccessRevisionConflict reports a stale cron policy revision.
// ErrCronPermissionDenied reports a missing definition or observation grant.
var ErrCronPermissionDenied = errors.New("cron permission denied")

// CronAccessCheck binds grant evaluation to authenticated request authority.
// Enforce is false only for daemon-owner or internal operations.
type CronAccessCheck struct {
	Actor   string
	Enforce bool
}

var ErrCronAccessRevisionConflict = errors.New("cron access revision conflict")

// CronGrant names the local teams allowed one cron operation.
type CronGrant struct {
	Mode     string   `json:"mode"`
	TeamUIDs []string `json:"team_uids"`
}

// ProjectCronAccessPolicy retains independent daemon-local cron grants.
type ProjectCronAccessPolicy struct {
	ProjectUID string    `json:"project_uid"`
	Define     CronGrant `json:"define"`
	Observe    CronGrant `json:"observe"`
	Revision   int64     `json:"revision"`
}

// CronAccess contains the actor's grants within project visibility.
type CronAccess struct {
	Define  bool `json:"define"`
	Observe bool `json:"observe"`
}

// CronAccessStorage administers local authority and returns the exact audit event.
type CronAccessStorage interface {
	CronAccessPolicy(context.Context, string) (ProjectCronAccessPolicy, error)
	SetCronAccessPolicy(context.Context, ProjectCronAccessPolicy, string) (ProjectCronAccessPolicy, Event, error)
	CronActorAccess(context.Context, string, string) (CronAccess, error)
}

type cronAccessDocument struct {
	Version  int       `json:"version"`
	Revision int64     `json:"revision"`
	Define   CronGrant `json:"define"`
	Observe  CronGrant `json:"observe"`
}

// NormalizeCronAccessPolicy validates and canonicalizes an API or typed backup
// policy. Revision zero is valid only for the initial compare-and-swap request.
func NormalizeCronAccessPolicy(policy ProjectCronAccessPolicy) (ProjectCronAccessPolicy, error) {
	if !uid.Valid(policy.ProjectUID) || policy.ProjectUID != strings.ToUpper(policy.ProjectUID) || policy.ProjectUID == SystemProjectUID || policy.Revision < 0 {
		return ProjectCronAccessPolicy{}, errors.New("invalid cron policy identity or revision")
	}
	for _, grant := range []*CronGrant{&policy.Define, &policy.Observe} {
		if grant.Mode != "any" && grant.Mode != "teams" {
			return ProjectCronAccessPolicy{}, errors.New("invalid cron grant mode")
		}
		if len(grant.TeamUIDs) > 64 || (grant.Mode == "any" && len(grant.TeamUIDs) > 0) {
			return ProjectCronAccessPolicy{}, errors.New("invalid cron grant teams")
		}
		grant.TeamUIDs = slices.Clone(grant.TeamUIDs)
		if grant.TeamUIDs == nil {
			grant.TeamUIDs = []string{}
		}
		slices.Sort(grant.TeamUIDs)
		for i, team := range grant.TeamUIDs {
			if !uid.Valid(team) || team != strings.ToUpper(team) || (i > 0 && grant.TeamUIDs[i-1] == team) {
				return ProjectCronAccessPolicy{}, errors.New("invalid or duplicate cron team UID")
			}
		}
	}
	return policy, nil
}

// DecodeCronAccessPolicy validates a present stored document. An invalid row
// never falls back to the unrestricted missing-row default.
func DecodeCronAccessPolicy(projectUID string, raw []byte) (ProjectCronAccessPolicy, error) {
	var doc cronAccessDocument
	if err := json.Unmarshal(raw, &doc, json.RejectUnknownMembers(true)); err != nil {
		return ProjectCronAccessPolicy{}, err
	}
	if doc.Version != 1 || doc.Revision <= 0 {
		return ProjectCronAccessPolicy{}, errors.New("invalid cron policy version or revision")
	}
	return NormalizeCronAccessPolicy(ProjectCronAccessPolicy{ProjectUID: projectUID, Define: doc.Define, Observe: doc.Observe, Revision: doc.Revision})
}

type cronAccessQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ReadCronAccessPolicy reads the exact project key through a caller-owned query
// surface, permitting authorization to stay in the mutation transaction.
func ReadCronAccessPolicy(ctx context.Context, q cronAccessQuery, projectUID string) (ProjectCronAccessPolicy, error) {
	var found string
	err := q.QueryRowContext(ctx, `SELECT uid FROM projects WHERE uid=$1 AND uid<>$2`, projectUID, SystemProjectUID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectCronAccessPolicy{}, ErrNotFound
	}
	if err != nil {
		return ProjectCronAccessPolicy{}, err
	}
	var raw string
	err = q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, CronAccessPolicyPrefix+projectUID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectCronAccessPolicy{ProjectUID: projectUID, Define: CronGrant{Mode: "any", TeamUIDs: []string{}}, Observe: CronGrant{Mode: "any", TeamUIDs: []string{}}}, nil
	}
	if err != nil {
		return ProjectCronAccessPolicy{}, err
	}
	return DecodeCronAccessPolicy(projectUID, []byte(raw))
}

// SetCronAccessPolicyTx checks mandatory CAS and references then stores the new
// document. The caller holds the exclusive access epoch lock and appends audit.
func SetCronAccessPolicyTx(ctx context.Context, tx Transaction, policy ProjectCronAccessPolicy) (ProjectCronAccessPolicy, error) {
	policy, err := NormalizeCronAccessPolicy(policy)
	if err != nil {
		return ProjectCronAccessPolicy{}, err
	}
	if policy.Revision == math.MaxInt64 {
		return ProjectCronAccessPolicy{}, errors.New("cron policy revision exhausted")
	}
	current, err := ReadCronAccessPolicy(ctx, tx, policy.ProjectUID)
	if err != nil {
		return ProjectCronAccessPolicy{}, err
	}
	if policy.Revision != current.Revision {
		return ProjectCronAccessPolicy{}, ErrCronAccessRevisionConflict
	}
	for _, grant := range []CronGrant{policy.Define, policy.Observe} {
		for _, team := range grant.TeamUIDs {
			var found string
			if err := tx.QueryRowContext(ctx, `SELECT uid FROM teams WHERE uid=$1`, team).Scan(&found); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ProjectCronAccessPolicy{}, ErrNotFound
				}
				return ProjectCronAccessPolicy{}, err
			}
		}
	}
	policy.Revision++
	raw, err := json.Marshal(cronAccessDocument{Version: 1, Revision: policy.Revision, Define: policy.Define, Observe: policy.Observe})
	if err != nil {
		return ProjectCronAccessPolicy{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, CronAccessPolicyPrefix+policy.ProjectUID, string(raw))
	return policy, err
}

// ReadCronActorAccess evaluates grants and current project visibility using the
// same query surface. Missing team references confer no authority.
func ReadCronActorAccess(ctx context.Context, q cronAccessQuery, projectUID, actor string) (CronAccess, error) {
	policy, err := ReadCronAccessPolicy(ctx, q, projectUID)
	if err != nil {
		return CronAccess{}, err
	}
	if actor != "" {
		if err := ValidateTokenActor(actor); err != nil {
			return CronAccess{}, err
		}
	}
	actor = strings.TrimSpace(actor)
	var visible bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects p LEFT JOIN project_access_policies a ON a.project_uid=p.uid WHERE p.uid=$1 AND (COALESCE(a.visibility,'all')='all' OR EXISTS(SELECT 1 FROM project_access_teams pat JOIN team_memberships tm ON tm.team_uid=pat.team_uid WHERE pat.project_uid=p.uid AND tm.actor=$2)))`, projectUID, actor).Scan(&visible)
	if err != nil || !visible {
		return CronAccess{}, err
	}
	holds := func(grant CronGrant) (bool, error) {
		if grant.Mode == "any" {
			return true, nil
		}
		if len(grant.TeamUIDs) == 0 {
			return false, nil
		}
		args := []any{actor}
		placeholders := make([]string, len(grant.TeamUIDs))
		for i, team := range grant.TeamUIDs {
			args = append(args, team)
			placeholders[i] = fmt.Sprintf("$%d", i+2)
		}
		var allowed bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_memberships m JOIN teams t ON t.uid=m.team_uid WHERE m.actor=$1 AND m.team_uid IN (`+strings.Join(placeholders, ",")+`))`, args...).Scan(&allowed)
		return allowed, err
	}
	var access CronAccess
	access.Define, err = holds(policy.Define)
	if err != nil {
		return CronAccess{}, err
	}
	access.Observe, err = holds(policy.Observe)
	return access, err
}
