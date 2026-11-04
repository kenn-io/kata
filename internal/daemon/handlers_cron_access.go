package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerCronAccessHandlers(h huma.API, cfg ServerConfig) {
	path := "/api/v1/projects/{project_id}/cron/access"
	huma.Register(h, huma.Operation{OperationID: "getCronAccess", Method: http.MethodGet, Path: path}, func(ctx context.Context, in *api.CronProjectRequest) (*api.CronAccessResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		policy, err := cfg.DB.CronAccessPolicy(ctx, project.UID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.CronAccessResponse{}
		out.Body.Policy = policy
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "setCronAccess", Method: http.MethodPut, Path: path}, func(ctx context.Context, in *api.SetCronAccessRequest) (*api.CronAccessResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		policy, err := db.NormalizeCronAccessPolicy(db.ProjectCronAccessPolicy{ProjectUID: project.UID, Define: in.Body.Define, Observe: in.Body.Observe, Revision: in.Body.Revision})
		if err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		for _, grant := range []db.CronGrant{policy.Define, policy.Observe} {
			if err := validateInitialTeamAccess(ctx, cfg.DB, grant.TeamUIDs); err != nil {
				return nil, err
			}
		}
		policy, event, err := cfg.DB.SetCronAccessPolicy(ctx, policy, tokenAdminAuditActor(ctx, db.BootstrapActor))
		if errors.Is(err, db.ErrCronAccessRevisionConflict) {
			return nil, api.NewError(409, "revision_conflict", "cron access policy changed", "reload the current policy before editing", nil)
		}
		if err != nil {
			return nil, projectAccessAPIError(err)
		}
		cfg.Publish().Event(event.ProjectID, event)
		out := &api.CronAccessResponse{}
		out.Body.Policy = policy
		out.Body.Event = &event
		return out, nil
	})
}

type cronAccessContextKey struct{}

// cronRequestAccess carries the admitted identity rather than user attribution.
// Enrollment handlers attach their independently authenticated hop identity.
func cronRequestAccess(ctx context.Context) db.CronAccessCheck {
	if check, ok := ctx.Value(cronAccessContextKey{}).(db.CronAccessCheck); ok {
		return check
	}
	if projectOwnerAuthority(ctx) {
		return db.CronAccessCheck{}
	}
	principal, _ := PrincipalFromContext(ctx)
	return db.CronAccessCheck{Actor: principal.Actor, Enforce: true}
}

func cronPermissionAPIError(err error, status int) error {
	grant := "required"
	if denied, ok := errors.AsType[*db.CronPermissionError](err); ok {
		grant = denied.Grant
	}
	return api.NewError(status, "cron_permission_denied", "cron permission denied", "ask the daemon owner to add you to a team with the "+grant+" grant (kata cron access show <project>)", nil)
}

func effectiveCronAccess(ctx context.Context, cfg ServerConfig, project db.Project) (db.CronAccess, error) {
	if !effectiveUIPolicy(ctx, cfg).Capabilities.Writable || ensureAttributedWriteAllowed(ctx) != nil {
		return db.CronAccess{}, nil
	}
	// Host authorization has no side-effect-free write-capability projection.
	// Do not invoke mutation authorization or transaction fences on this GET.
	if hostAccessGoverns(ctx) {
		return db.CronAccess{}, nil
	}
	binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return db.CronAccess{}, err
	}
	if err == nil && binding.Enabled && binding.Role == db.FederationRoleSpoke && !binding.PushEnabled {
		return db.CronAccess{}, nil
	}
	check := cronRequestAccess(ctx)
	if !check.Enforce {
		return db.CronAccess{Define: true, Observe: true}, nil
	}
	return cfg.DB.CronActorAccess(ctx, project.UID, check.Actor)
}
