package daemon

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// activeProjectByID resolves a project by rowid for surface API handlers
// that should treat archived projects as not-found. Returns the api.NewError
// envelope directly so call sites can `return nil, err`.
//
// Internal helpers (merge, restore, alias resolve) that need to operate on
// archived rows must use store.ProjectByID directly.
func activeProjectByID(ctx context.Context, store db.Storage, id int64) (db.Project, error) {
	p, err := store.ProjectByID(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Project{}, api.NewError(404, "project_not_found", "project not found", "", nil)
		}
		return db.Project{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if p.DeletedAt != nil {
		return db.Project{}, api.NewError(404, "project_not_found", "project not found", "", nil)
	}
	return p, nil
}

// dbProjectToOut maps a db.Project (internal row) to the API-shape
// ProjectOut. Metadata carries the JSON blob verbatim so consumers can
// read keys like `area` without a follow-up fetch. Stats stays nil —
// that field is populated only by the list-projects handler when
// ?include=stats is set (Task 3).
func dbProjectToOut(p db.Project) api.ProjectOut {
	return api.ProjectOut{
		ID:        p.ID,
		UID:       p.UID,
		Name:      p.Name,
		Metadata:  p.Metadata,
		Revision:  p.Revision,
		CreatedAt: p.CreatedAt,
		DeletedAt: p.DeletedAt,
	}
}

// includeContains reports whether the comma-separated ?include= value
// names the given token. Whitespace is trimmed; matching is case-
// insensitive on the token side. Spec §7.1.
func includeContains(includeParam, token string) bool {
	for _, part := range strings.Split(includeParam, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// registerProjectsHandlers installs project-scoped routes (resolve, init, list,
// show) on humaAPI. Resolution and init semantics live entirely on the daemon
// per spec §2.4 so all clients (CLI, TUI, future) see identical behavior.
func registerProjectsHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "resolveProject",
		Method:      "POST",
		Path:        "/api/v1/projects/resolve",
	}, func(ctx context.Context, in *api.ResolveProjectRequest) (*api.ResolveProjectResponse, error) {
		if resolveProjectRequestCanMutate(in) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
		}
		out, err := resolveProject(ctx, cfg.DB, in.Body.Alias, in.Body.Name, in.Body.StartPath)
		if err != nil {
			return nil, err
		}
		return &api.ResolveProjectResponse{Body: *out}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "initProject",
		Method:      "POST",
		Path:        "/api/v1/projects",
	}, func(ctx context.Context, in *api.InitProjectRequest) (*api.InitProjectResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		out, created, err := initProject(ctx, cfg.DB, in)
		if err != nil {
			return nil, err
		}
		resp := &api.InitProjectResponse{}
		resp.Body.ProjectResolveBody = *out
		resp.Body.Created = created
		return resp, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "listProjects",
		Method:      "GET",
		Path:        "/api/v1/projects",
	}, func(ctx context.Context, in *struct {
		Include string `query:"include"`
	}) (*api.ListProjectsResponse, error) {
		var (
			ps  []db.Project
			err error
		)
		if includeContains(in.Include, "archived") {
			ps, err = cfg.DB.ListProjectsIncludingArchived(ctx)
		} else {
			ps, err = cfg.DB.ListProjects(ctx)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		outs := make([]api.ProjectOut, len(ps))
		for i, p := range ps {
			outs[i] = dbProjectToOut(p)
		}
		if includeContains(in.Include, "stats") {
			stats, err := cfg.DB.BatchProjectStats(ctx)
			if err != nil {
				return nil, api.NewError(500, "internal", err.Error(), "", nil)
			}
			for i, p := range ps {
				if s, ok := stats[p.ID]; ok {
					outs[i].Stats = &api.ProjectStatsOut{
						Open:        s.Open,
						Closed:      s.Closed,
						LastEventAt: s.LastEventAt,
					}
				}
			}
		}
		out := &api.ListProjectsResponse{}
		out.Body.Projects = outs
		return out, nil
	})

	// /reset-counter endpoint removed: spec §9.5 drops the per-project
	// issue counter, so there is no value to reset.

	huma.Register(humaAPI, huma.Operation{
		OperationID: "showProject",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}",
	}, func(ctx context.Context, in *struct {
		ProjectID int64 `path:"project_id"`
	}) (*api.ShowProjectResponse, error) {
		p, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		aliases, err := cfg.DB.ProjectAliases(ctx, p.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.ShowProjectResponse{}
		out.Body.Project = dbProjectToOut(p)
		out.Body.Aliases = aliases
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "mergeProject",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/merge",
	}, func(ctx context.Context, in *api.MergeProjectRequest) (*api.MergeProjectResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		if in.Body.SourceProjectID == 0 {
			return nil, api.NewError(400, "validation", "source_project_id required", "", nil)
		}
		targetName := strings.TrimSpace(in.Body.TargetName)
		var namePtr *string
		if targetName != "" {
			namePtr = &targetName
		}
		merged, err := cfg.DB.MergeProjects(ctx, db.MergeProjectsParams{
			SourceProjectID: in.Body.SourceProjectID,
			TargetProjectID: in.ProjectID,
			TargetName:      namePtr,
		})
		if errors.Is(err, db.ErrProjectMergeSameProject) {
			return nil, api.NewError(400, "validation", "cannot merge a project into itself", "", nil)
		}
		// project_merge_issue_number_collision is gone — short_id collisions
		// auto-extend in the db layer (spec §9.4).
		var mappingCollision *db.ProjectMergeImportMappingCollisionError
		if errors.As(err, &mappingCollision) {
			return nil, api.NewError(409, "project_merge_import_mapping_collision",
				"source and target have overlapping import mappings",
				"resolve import mapping collisions before merging", map[string]any{"mappings": mappingCollision.Mappings})
		}
		if errors.Is(err, db.ErrProjectMergeArchivedSource) {
			return nil, api.NewError(409, "project_merge_archived_source",
				"source project is archived", "", nil)
		}
		if errors.Is(err, db.ErrProjectMergeArchivedTarget) {
			return nil, api.NewError(409, "project_merge_archived_target",
				"target project is archived", "", nil)
		}
		if errors.Is(err, db.ErrProjectMergeFederationBinding) {
			return nil, api.NewError(409, "project_merge_federation_binding",
				"cannot merge federated projects", "", nil)
		}
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "project_not_found", "project not found", "", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		extensions := make([]api.MergeShortIDExtension, 0, len(merged.ShortIDExtensions))
		for _, ext := range merged.ShortIDExtensions {
			extensions = append(extensions, api.MergeShortIDExtension{
				UID:              ext.UID,
				PreMergeShortID:  ext.PreMergeShortID,
				PostMergeShortID: ext.PostMergeShortID,
			})
		}
		return &api.MergeProjectResponse{Body: api.MergeProjectResultOut{
			Source:            dbProjectToOut(merged.Source),
			Target:            dbProjectToOut(merged.Target),
			IssuesMoved:       merged.IssuesMoved,
			AliasesMoved:      merged.AliasesMoved,
			EventsMoved:       merged.EventsMoved,
			PurgeLogsMoved:    merged.PurgeLogsMoved,
			ShortIDExtensions: extensions,
		}}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "removeProject",
		Method:      "DELETE",
		Path:        "/api/v1/projects/{project_id}",
	}, func(ctx context.Context, in *api.RemoveProjectRequest) (*api.RemoveProjectResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		project, evt, err := cfg.DB.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: in.ProjectID, Actor: actor, Force: in.Force,
		})
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(404, "project_not_found", "project not found", "", nil)
		case errors.Is(err, db.ErrProjectAlreadyArchived):
			return nil, api.NewError(409, "project_already_archived",
				"project is already archived", "", nil)
		}
		var openErr *db.ProjectHasOpenIssuesError
		if errors.As(err, &openErr) {
			return nil, api.NewError(409, "project_has_open_issues",
				"project has open issues",
				"close or purge the open issues first, or pass force=true",
				map[string]any{"open_issues": openErr.OpenIssues})
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: project.ID})
		cfg.Hooks.Enqueue(*evt)
		out := &api.RemoveProjectResponse{}
		out.Body.Project = dbProjectToOut(project)
		out.Body.Event = evt
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "restoreProject",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/restore",
	}, func(ctx context.Context, in *api.RestoreProjectRequest) (*api.RestoreProjectResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		project, evt, changed, err := cfg.DB.RestoreProject(ctx, in.ProjectID, actor)
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "project_not_found", "project not found", "", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if changed && evt != nil {
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: project.ID})
			cfg.Hooks.Enqueue(*evt)
		}
		out := &api.RestoreProjectResponse{}
		out.Body.Project = dbProjectToOut(project)
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "detachProjectAlias",
		Method:      "DELETE",
		Path:        "/api/v1/projects/{project_id}/aliases/{alias_id}",
	}, func(ctx context.Context, in *api.DetachProjectAliasRequest) (*api.DetachProjectAliasResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		// (project_id, alias_id) is validated atomically inside the delete
		// transaction so a reassignment between any preflight and the delete
		// cannot drop an alias from a different project than the request named.
		alias, evt, err := cfg.DB.DetachProjectAlias(ctx, db.DetachAliasParams{
			ProjectID: in.ProjectID, AliasID: in.AliasID, Actor: actor, Force: in.Force,
		})
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(404, "alias_not_found",
				"alias not found for the requested project", "", nil)
		case errors.Is(err, db.ErrAliasIsLast):
			return nil, api.NewError(409, "alias_is_last",
				"alias is the only one for its project",
				"detach with force=true to drop it anyway, or attach a replacement first", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: alias.ProjectID})
		cfg.Hooks.Enqueue(*evt)
		out := &api.DetachProjectAliasResponse{}
		out.Body.Alias = alias
		out.Body.Event = evt
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "renameProject",
		Method:      "PATCH",
		Path:        "/api/v1/projects/{project_id}",
	}, func(ctx context.Context, in *api.RenameProjectRequest) (*api.ShowProjectResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(in.Body.Name)
		if err := config.ValidateProjectName(name); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		p, err := cfg.DB.RenameProject(ctx, in.ProjectID, name)
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "project_not_found", "project not found", "", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		aliases, err := cfg.DB.ProjectAliases(ctx, p.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.ShowProjectResponse{}
		out.Body.Project = dbProjectToOut(p)
		out.Body.Aliases = aliases
		return out, nil
	})
}

func resolveProjectRequestCanMutate(in *api.ResolveProjectRequest) bool {
	return in.Body.Alias != nil || strings.TrimSpace(in.Body.StartPath) != ""
}

// resolveProject implements project resolution. Inputs are tried in
// priority order: alias (path-free, with optional name fallback +
// first-seen attach), then bare name (strict path-free lookup), then
// start_path (legacy local-daemon walk). Resolve never creates
// projects: alias misses without a name match return 404.
func resolveProject(ctx context.Context, store db.Storage, alias *api.AliasInput, name, startPath string) (*api.ProjectResolveBody, error) {
	if alias != nil {
		return resolveByAliasInput(ctx, store, alias, name)
	}
	if name != "" {
		return resolveByName(ctx, store, name)
	}
	if startPath == "" {
		return nil, api.NewError(400, "validation",
			"one of alias, name, or start_path is required", "", nil)
	}
	abs, err := filepath.Abs(startPath)
	if err != nil {
		return nil, api.NewError(400, "validation", err.Error(), "", nil)
	}
	disc, err := config.DiscoverPaths(abs)
	if err != nil {
		return nil, api.NewError(400, "validation", err.Error(), "", nil)
	}

	if body, ok, err := resolveByKataToml(ctx, store, disc); err != nil {
		return nil, err
	} else if ok {
		return body, nil
	}

	if disc.GitRoot != "" {
		return resolveByAlias(ctx, store, disc)
	}

	return nil, api.NewError(404, "project_not_initialized",
		"no .kata.toml ancestor and no git ancestor",
		`run "kata init" inside a workspace`, nil)
}

// resolveByAliasInput handles the alias-aware path-free resolve flow.
// The daemon never touches the client filesystem: alias.identity is the
// canonical key; alias.root_path is stored as opaque metadata. When the
// alias is unknown but a name is supplied, the daemon falls back to
// name lookup and attaches the alias on first-seen — so a remote client
// resolving against a daemon that was previously only reachable from a
// different host upgrades to the alias-first path on the next call.
// Resolve never creates projects: an unknown alias with an unknown (or
// absent) name returns 404.
func resolveByAliasInput(ctx context.Context, store db.Storage, in *api.AliasInput, name string) (*api.ProjectResolveBody, error) {
	info := config.AliasInfo{
		Identity: in.Identity,
		Kind:     in.Kind,
		RootPath: in.RootPath,
	}
	if err := config.ValidateAliasInfo(info); err != nil {
		return nil, api.NewError(400, "validation", err.Error(), "", nil)
	}
	alias, err := store.AliasByIdentity(ctx, info.Identity)
	switch {
	case err == nil:
		// Fetch the project before touching the alias so an alias
		// pointing at an archived row (theoretically possible via
		// import or direct DB edits — RemoveProject normally hard-
		// deletes aliases atomically) doesn't bump last_seen_at on the
		// stale binding.
		project, err := store.ProjectByID(ctx, alias.ProjectID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if project.DeletedAt != nil {
			return nil, api.NewError(404, "project_not_initialized",
				"alias "+info.Identity+" points at an archived project",
				`run "kata init" to bind this workspace to an active project`, nil)
		}
		if err := store.TouchAlias(ctx, alias.ID, info.RootPath); err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if refreshed, err := store.AliasByIdentity(ctx, info.Identity); err == nil {
			alias = refreshed
		}
		return &api.ProjectResolveBody{
			Project:       dbProjectToOut(project),
			Alias:         alias,
			WorkspaceRoot: info.RootPath,
		}, nil
	case !errors.Is(err, db.ErrNotFound):
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}

	// Alias unknown. Fall back to name lookup if supplied so a fresh
	// client (new host or fresh checkout) can still resolve when the
	// project was registered without this alias.
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, api.NewError(404, "project_not_initialized",
			"alias "+info.Identity+" is not registered",
			`run "kata init" in this workspace`, nil)
	}
	if err := config.ValidateProjectName(name); err != nil {
		return nil, api.NewError(400, "validation", err.Error(), "", nil)
	}
	project, err := store.ProjectByName(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return nil, api.NewError(404, "project_not_initialized",
			"project "+name+" is not registered",
			`run "kata init" in this workspace`, nil)
	}
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	// First-seen attach: bind the supplied alias to the matched project
	// so subsequent resolves hit the alias path. Reassign=false matches
	// resolve's strict-lookup contract; an existing alias bound
	// elsewhere surfaces as 409 rather than silently moving.
	attached, err := attachAlias(ctx, store, project.ID, info, false)
	if err != nil {
		return nil, err
	}
	return &api.ProjectResolveBody{
		Project:       dbProjectToOut(project),
		Alias:         attached,
		WorkspaceRoot: info.RootPath,
	}, nil
}

func resolveByName(ctx context.Context, store db.Storage, name string) (*api.ProjectResolveBody, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, api.NewError(400, "validation", "name must be non-empty", "", nil)
	}
	if err := config.ValidateProjectName(name); err != nil {
		return nil, api.NewError(400, "validation", err.Error(), "", nil)
	}
	project, err := store.ProjectByName(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return nil, api.NewError(404, "project_not_initialized",
			"project "+name+" is not registered",
			`run "kata init" in this workspace`, nil)
	}
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return &api.ProjectResolveBody{Project: dbProjectToOut(project)}, nil
}

// resolveByKataToml returns (body, true, nil) when a .kata.toml binding
// exists at the workspace root and resolves to a project. Returns
// (nil, false, nil) when there is no .kata.toml. Surfaces parse errors.
func resolveByKataToml(ctx context.Context, store db.Storage, disc config.DiscoveredPaths) (*api.ProjectResolveBody, bool, error) {
	if disc.WorkspaceRoot == "" {
		return nil, false, nil
	}
	cfgFile, err := config.ReadProjectConfig(disc.WorkspaceRoot)
	if err != nil {
		if errors.Is(err, config.ErrProjectConfigMissing) {
			return nil, false, nil
		}
		return nil, false, api.NewError(400, "validation", err.Error(), "", nil)
	}
	if body, ok, err := resolveByAliasIfAvailable(ctx, store, disc); err != nil {
		return nil, false, err
	} else if ok {
		if body == nil {
			return nil, false, api.NewError(500, "internal", "alias resolver returned no project", "", nil)
		}
		if cfgFile.Project.Name != "" && body.Project.Name != cfgFile.Project.Name {
			if err := config.WriteProjectConfig(disc.WorkspaceRoot, body.Project.Name); err != nil {
				return nil, false, api.NewError(500, "internal", err.Error(), "", nil)
			}
		}
		return body, true, nil
	}
	project, err := store.ProjectByName(ctx, cfgFile.Project.Name)
	if errors.Is(err, db.ErrNotFound) {
		return nil, false, api.NewError(404, "project_not_initialized",
			"project "+cfgFile.Project.Name+" is bound by .kata.toml but not registered",
			`run "kata init" in this workspace`, nil)
	}
	if err != nil {
		return nil, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	alias, err := upsertAliasFor(ctx, store, project.ID, disc, false)
	if err != nil {
		return nil, false, err
	}
	return &api.ProjectResolveBody{
		Project:       dbProjectToOut(project),
		Alias:         alias,
		WorkspaceRoot: disc.WorkspaceRoot,
	}, true, nil
}

func resolveByAliasIfAvailable(ctx context.Context, store db.Storage, disc config.DiscoveredPaths) (*api.ProjectResolveBody, bool, error) {
	if disc.GitRoot == "" && disc.WorkspaceRoot == "" {
		return nil, false, nil
	}
	info, err := config.ComputeAliasIdentity(ctx, disc)
	if err != nil {
		return nil, false, api.NewError(400, "validation", err.Error(), "", nil)
	}
	alias, err := store.AliasByIdentity(ctx, info.Identity)
	if errors.Is(err, db.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if err := store.TouchAlias(ctx, alias.ID, info.RootPath); err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if refreshed, err := store.AliasByIdentity(ctx, info.Identity); err == nil {
		alias = refreshed
	}
	project, err := store.ProjectByID(ctx, alias.ProjectID)
	if err != nil {
		return nil, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return &api.ProjectResolveBody{
		Project:       dbProjectToOut(project),
		Alias:         alias,
		WorkspaceRoot: info.RootPath,
	}, true, nil
}

// resolveByAlias looks up the alias derived from the git root and returns
// the bound project. Caller guarantees disc.GitRoot != "".
func resolveByAlias(ctx context.Context, store db.Storage, disc config.DiscoveredPaths) (*api.ProjectResolveBody, error) {
	info, err := config.ComputeAliasIdentity(ctx, disc)
	if err != nil {
		return nil, api.NewError(400, "validation", err.Error(), "", nil)
	}
	alias, err := store.AliasByIdentity(ctx, info.Identity)
	if errors.Is(err, db.ErrNotFound) {
		return nil, api.NewError(404, "project_not_initialized",
			"no kata project is attached to this workspace",
			`run "kata init" in this workspace`, nil)
	}
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if err := store.TouchAlias(ctx, alias.ID, info.RootPath); err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	// Refetch the alias so the response carries the updated last_seen_at and
	// root_path, not the pre-touch snapshot.
	if refreshed, err := store.AliasByIdentity(ctx, info.Identity); err == nil {
		alias = refreshed
	}
	project, err := store.ProjectByID(ctx, alias.ProjectID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return &api.ProjectResolveBody{
		Project:       dbProjectToOut(project),
		Alias:         alias,
		WorkspaceRoot: info.RootPath,
	}, nil
}

func initProject(ctx context.Context, store db.Storage, req *api.InitProjectRequest) (*api.ProjectResolveBody, bool, error) {
	if req.Body.StartPath == "" {
		if req.Body.Name == "" {
			return nil, false, api.NewError(400, "validation",
				"either name or start_path is required", "", nil)
		}
		return initByName(ctx, store, req)
	}
	abs, err := filepath.Abs(req.Body.StartPath)
	if err != nil {
		return nil, false, api.NewError(400, "validation", err.Error(), "", nil)
	}
	disc, err := config.DiscoverPaths(abs)
	if err != nil {
		return nil, false, api.NewError(400, "validation", err.Error(), "", nil)
	}

	tomlCfg, err := readWorkspaceConfig(disc)
	if err != nil {
		return nil, false, err
	}

	name, err := pickInitName(ctx, req, disc, tomlCfg)
	if err != nil {
		return nil, false, err
	}

	// When --project was supplied outside any git/workspace ancestor, synthesize
	// a local alias rooted at the start path so upsertAliasFor has something to
	// attach. This is the explicit escape hatch documented in spec §2.4.
	if disc.GitRoot == "" && disc.WorkspaceRoot == "" {
		disc.WorkspaceRoot = abs
	}

	aliasInfo, err := config.ComputeAliasIdentity(ctx, disc)
	if err != nil {
		return nil, false, api.NewError(400, "validation", err.Error(), "", nil)
	}
	explicitName := strings.TrimSpace(req.Body.Name) != ""
	existingAlias, err := store.AliasByIdentity(ctx, aliasInfo.Identity)
	if err == nil {
		if !explicitName {
			if existingProject, err := store.ProjectByID(ctx, existingAlias.ProjectID); err == nil && existingProject.DeletedAt == nil {
				name = existingProject.Name
			}
		}
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	// Preflight alias conflict before mutating anything: without this, a fresh
	// project row would be created and then orphaned when alias attach fails.
	if err := preflightAliasConflict(ctx, store, aliasInfo, name, req.Body.Reassign); err != nil {
		return nil, false, err
	}

	project, created, err := upsertProject(ctx, store, name)
	if err != nil {
		return nil, false, err
	}

	alias, err := attachAlias(ctx, store, project.ID, aliasInfo, req.Body.Reassign)
	if err != nil {
		// Concurrent init can race past the preflight: a parallel request can
		// insert the alias between our preflight check and our attach. The
		// preflight catches the no-race case; here we clean up the orphan
		// project row so retries observe consistent state regardless of which
		// failure we hit.
		if created {
			_ = store.HardDeleteProject(ctx, project.ID)
		}
		return nil, false, err
	}

	dest := config.WriteDestination(disc, abs)
	if tomlCfg == nil || tomlCfg.Project.Name != project.Name {
		if err := config.WriteProjectConfig(dest, project.Name); err != nil {
			return nil, false, api.NewError(500, "internal", err.Error(), "", nil)
		}
	}

	return &api.ProjectResolveBody{
		Project:       dbProjectToOut(project),
		Alias:         alias,
		WorkspaceRoot: dest,
	}, created, nil
}

func initByName(ctx context.Context, store db.Storage, req *api.InitProjectRequest) (*api.ProjectResolveBody, bool, error) {
	name := strings.TrimSpace(req.Body.Name)
	if err := config.ValidateProjectName(name); err != nil {
		return nil, false, api.NewError(400, "validation", err.Error(), "", nil)
	}
	if req.Body.Reassign && req.Body.Alias == nil {
		return nil, false, api.NewError(400, "validation",
			"reassign requires alias metadata in path-free init",
			"omit --reassign or run from a workspace where the client can derive an alias", nil)
	}

	var aliasInfo *config.AliasInfo
	if req.Body.Alias != nil {
		info := config.AliasInfo{
			Identity: req.Body.Alias.Identity,
			Kind:     req.Body.Alias.Kind,
			RootPath: req.Body.Alias.RootPath,
		}
		// ValidateAliasInfo applies kind-aware rules: git aliases get
		// the project-identity charset check; local aliases (which
		// carry workspace paths) only need a non-empty path. Without
		// this, workspaces like "/Users/me/My Project" — perfectly
		// valid for path-based init — would be rejected here on the
		// whitespace check.
		if err := config.ValidateAliasInfo(info); err != nil {
			return nil, false, api.NewError(400, "validation", err.Error(), "", nil)
		}
		// Preflight before any project mutation, mirroring path-based
		// init: avoids creating an orphan project row when the alias
		// would conflict.
		if err := preflightAliasConflict(ctx, store, info, name, req.Body.Reassign); err != nil {
			return nil, false, err
		}
		aliasInfo = &info
	}

	project, created, err := upsertProject(ctx, store, name)
	if err != nil {
		return nil, false, err
	}

	body := &api.ProjectResolveBody{Project: dbProjectToOut(project)}
	if aliasInfo != nil {
		alias, err := attachAlias(ctx, store, project.ID, *aliasInfo, req.Body.Reassign)
		if err != nil {
			// Concurrent init can race past the preflight (alias
			// inserted between our check and our attach). Clean up
			// the orphan project row so retries observe consistent
			// state, matching the path-based recovery.
			if created {
				_ = store.HardDeleteProject(ctx, project.ID)
			}
			return nil, false, err
		}
		body.Alias = alias
	}
	return body, created, nil
}

// readWorkspaceConfig reads .kata.toml only when a workspace root was actually
// discovered; passing "" to ReadProjectConfig would resolve to the daemon's
// cwd. Parse errors surface as 400; "missing" returns nil.
func readWorkspaceConfig(disc config.DiscoveredPaths) (*config.ProjectConfig, error) {
	if disc.WorkspaceRoot == "" {
		return nil, nil
	}
	cfgFile, err := config.ReadProjectConfig(disc.WorkspaceRoot)
	if err != nil {
		if errors.Is(err, config.ErrProjectConfigMissing) {
			return nil, nil
		}
		return nil, api.NewError(400, "validation", err.Error(), "", nil)
	}
	return cfgFile, nil
}

func pickInitName(ctx context.Context, req *api.InitProjectRequest, disc config.DiscoveredPaths, tomlCfg *config.ProjectConfig) (string, error) {
	choice, err := config.PickInitName(ctx, disc, tomlCfg, req.Body.Name, req.Body.Replace)
	switch {
	case errors.Is(err, config.ErrNameConflict):
		return "", api.NewError(http.StatusConflict, "project_binding_conflict",
			err.Error(), "pass replace=true to overwrite", nil)
	case errors.Is(err, config.ErrNoNameSource):
		return "", api.NewError(400, "validation",
			err.Error(), `pass name or run inside a workspace`, nil)
	case err != nil:
		return "", api.NewError(400, "validation", err.Error(), "", nil)
	}
	if err := config.ValidateProjectName(choice.Name); err != nil {
		return "", api.NewError(400, "validation", err.Error(), "", nil)
	}
	return choice.Name, nil
}

func upsertProject(ctx context.Context, store db.Storage, name string) (db.Project, bool, error) {
	got, err := store.ProjectByName(ctx, name)
	if err == nil {
		return got, false, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return db.Project{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if archived, archErr := store.ProjectByNameIncludingArchived(ctx, name); archErr == nil && archived.DeletedAt != nil {
		return db.Project{}, false, api.NewError(409, "project_archived",
			"project with this name was archived via `kata projects remove`",
			`run "kata projects restore `+name+`" or pick a different name`,
			map[string]any{"name": name, "deleted_at": archived.DeletedAt})
	}
	created, err := store.CreateProject(ctx, name)
	if err != nil {
		return db.Project{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return created, true, nil
}

// upsertAliasFor is the disc-flavored entry point used during resolve, where
// no preflight has happened. It computes the alias identity then delegates.
func upsertAliasFor(ctx context.Context, store db.Storage, projectID int64, disc config.DiscoveredPaths, reassign bool) (db.ProjectAlias, error) {
	info, err := config.ComputeAliasIdentity(ctx, disc)
	if err != nil {
		return db.ProjectAlias{}, api.NewError(400, "validation", err.Error(), "", nil)
	}
	return attachAlias(ctx, store, projectID, info, reassign)
}

// attachAlias attaches a pre-computed alias identity to projectID. If the
// alias is already attached to a *different* project, returns
// project_alias_conflict (409) unless reassign is true (in which case we move
// it). When called after preflightAliasConflict, the conflict branch is
// unreachable but kept for callers that haven't preflit.
func attachAlias(ctx context.Context, store db.Storage, projectID int64, info config.AliasInfo, reassign bool) (db.ProjectAlias, error) {
	existing, err := store.AliasByIdentity(ctx, info.Identity)
	if err == nil {
		return applyExistingAlias(ctx, store, projectID, info, existing, reassign)
	}
	if !errors.Is(err, db.ErrNotFound) {
		return db.ProjectAlias{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	a, err := store.AttachAlias(ctx, projectID, info.Identity, info.Kind, info.RootPath)
	if err != nil {
		// A UNIQUE constraint failure on alias_identity means a concurrent init
		// beat us to the insert. Refetch the now-existing alias and apply the
		// same existing-alias logic: idempotent if it points to this project,
		// conflict or reassign otherwise.
		if strings.Contains(err.Error(), "UNIQUE constraint failed: project_aliases.alias_identity") {
			raced, refetchErr := store.AliasByIdentity(ctx, info.Identity)
			if refetchErr != nil {
				return db.ProjectAlias{}, api.NewError(500, "internal",
					"alias UNIQUE conflict but refetch failed: "+refetchErr.Error(), "", nil)
			}
			return applyExistingAlias(ctx, store, projectID, info, raced, reassign)
		}
		return db.ProjectAlias{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return a, nil
}

// applyExistingAlias handles the case where an alias row already exists.
// If the alias belongs to projectID it is touched (last_seen updated) and
// returned — enabling idempotent concurrent inits. Otherwise the alias is
// either moved (reassign=true) or a 409 is returned.
func applyExistingAlias(ctx context.Context, store db.Storage, projectID int64, info config.AliasInfo, existing db.ProjectAlias, reassign bool) (db.ProjectAlias, error) {
	if existing.ProjectID == projectID {
		if err := store.TouchAlias(ctx, existing.ID, info.RootPath); err != nil && !errors.Is(err, db.ErrNotFound) {
			return db.ProjectAlias{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		refreshed, _ := store.AliasByIdentity(ctx, info.Identity)
		return refreshed, nil
	}
	if !reassign {
		return db.ProjectAlias{}, api.NewError(http.StatusConflict, "project_alias_conflict",
			"alias already attached to a different project",
			"pass reassign=true to move it", map[string]any{
				"alias_identity":      info.Identity,
				"existing_project_id": existing.ProjectID,
			})
	}
	if execErr := store.ReassignAlias(ctx, existing.ID, projectID, info.RootPath); execErr != nil {
		return db.ProjectAlias{}, api.NewError(500, "internal", execErr.Error(), "", nil)
	}
	refreshed, _ := store.AliasByIdentity(ctx, info.Identity)
	return refreshed, nil
}

// preflightAliasConflict returns 409 project_alias_conflict when an existing
// alias is bound to a different project than targetName and reassign is
// false. Run before any project mutation so a doomed init does not leave an
// orphan project row.
func preflightAliasConflict(ctx context.Context, store db.Storage, info config.AliasInfo, targetName string, reassign bool) error {
	if reassign {
		return nil
	}
	existing, err := store.AliasByIdentity(ctx, info.Identity)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	existingProject, err := store.ProjectByID(ctx, existing.ProjectID)
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	if existingProject.Name == targetName {
		return nil
	}
	targetProject, err := store.ProjectByName(ctx, targetName)
	if err == nil && targetProject.ID == existing.ProjectID {
		return nil
	}
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	return api.NewError(http.StatusConflict, "project_alias_conflict",
		"alias already attached to a different project",
		"pass reassign=true to move it", map[string]any{
			"alias_identity":      info.Identity,
			"existing_project_id": existing.ProjectID,
		})
}
