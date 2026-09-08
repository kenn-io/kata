package daemon

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/api"
)

type projectTargetRequest interface {
	Target() *api.ProjectTarget
}

type projectNameResponse interface {
	SetProjectName(string)
}

// withResolvedProject keeps project selection inside the mutation request.
// HostAccess authorizes name selectors as all-project operations before this
// handler runs, matching the standalone resolve endpoint's authority.
func withResolvedProject[I any, O any, PI interface {
	*I
	projectTargetRequest
}, PO interface {
	*O
	projectNameResponse
}](cfg ServerConfig, handler func(context.Context, *I) (*O, error)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		target := PI(in).Target()
		name, named := strings.CutPrefix(target.ProjectSelector, "name:")
		var canonicalName string
		if named {
			var alias *api.AliasInput
			if target.ProjectAlias != "" || target.ProjectAliasKind != "" {
				if err := ensureAttributedWriteAllowed(ctx); err != nil {
					return nil, err
				}
				if principal, ok := PrincipalFromContext(ctx); ok && principal.Kind == PrincipalWebLocal {
					return nil, api.NewError(http.StatusForbidden, "web_local_operation_forbidden",
						"local web sessions cannot resolve projects through daemon aliases", "", nil)
				}
				alias = &api.AliasInput{Identity: target.ProjectAlias, Kind: target.ProjectAliasKind}
			}
			resolved, err := resolveProject(ctx, cfg.DB, alias, name, "")
			if err != nil {
				return nil, err
			}
			target.ProjectID = resolved.Project.ID
			canonicalName = resolved.Project.Name
		} else {
			if target.ProjectAlias != "" || target.ProjectAliasKind != "" {
				return nil, api.NewError(http.StatusBadRequest, "validation",
					"project alias headers require a name: project selector", "", nil)
			}
			id, err := strconv.ParseInt(target.ProjectSelector, 10, 64)
			if err != nil {
				return nil, api.NewError(http.StatusBadRequest, "validation",
					"project selector must be a numeric ID or name:<project name>", "", nil)
			}
			project, err := activeProjectByID(ctx, cfg.DB, id)
			if err != nil {
				return nil, err
			}
			target.ProjectID = project.ID
			canonicalName = project.Name
		}
		out, err := handler(ctx, in)
		if err != nil {
			return nil, err
		}
		PO(out).SetProjectName(canonicalName)
		return out, nil
	}
}
