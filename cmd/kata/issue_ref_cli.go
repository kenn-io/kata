package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
)

// resolveIssueRefForCommand parses one positional issue-ref argument, resolves
// the project it binds to into a numeric project ID, and returns the parsed
// pieces every ref-consuming command needs: the cobra context, the daemon
// base URL, the resolved project ID, and a resolvedIssueRef whose RefForAPI
// is the literal {ref} the URL path expects.
//
// A qualified ref ("kata#abc4") overrides the workspace's project binding so
// `kata show kata#abc4` works from any workspace. A bare short_id / ULID
// inherits the workspace's bound project. When neither source supplies a
// project name, the daemon is asked to resolve from the workspace's
// .kata.toml binding via start_path.
func resolveIssueRefForCommand(cmd *cobra.Command, ref string) (context.Context, string, int64, resolvedIssueRef, error) {
	return resolveIssueRefForCommandWithOptions(cmd, ref, false)
}

func resolveIssueRefForCommandWithOptions(cmd *cobra.Command, ref string, _ bool) (context.Context, string, int64, resolvedIssueRef, error) {
	ctx := cmd.Context()
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return nil, "", 0, resolvedIssueRef{}, err
	}
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return nil, "", 0, resolvedIssueRef{}, err
	}
	// Fallback chain for bare refs: --project flag → workspace binding → "".
	// An explicit --project must override the workspace binding so users
	// can target a different project from outside (or inside) a workspace
	// without needing to qualify every ref. ResolveRef errors when both
	// sources are empty — the caller hears "no project bound".
	bareProject := strings.TrimSpace(flags.Project)
	if bareProject == "" {
		bareProject = workspaceProjectName(start)
	}
	parsed, err := ResolveRef(ref, bareProject)
	if err != nil {
		return nil, "", 0, resolvedIssueRef{}, &cliError{
			Message:  err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	pid, projectName, err := resolveProjectIDAndNameForRef(ctx, baseURL, start, parsed.ProjectName)
	if err != nil {
		return nil, "", 0, resolvedIssueRef{}, err
	}
	return ctx, baseURL, pid, resolvedIssueRef{
		RefForAPI:   parsed.RefForAPI,
		ProjectName: projectName,
	}, nil
}

// resolveProjectIDAndNameForRef resolves the project ID + canonical project
// name needed by ref-consuming commands. A qualified ref (e.g. "kata#abc4")
// pins the project name; a bare ref / ULID inherits the workspace binding.
// The canonical name is used by destructive verbs to format the
// X-Kata-Confirm header value ("DELETE <project>#<short_id>").
func resolveProjectIDAndNameForRef(ctx context.Context, baseURL, startPath, refProjectName string) (int64, string, error) {
	if strings.TrimSpace(refProjectName) == "" {
		return resolveProjectIDAndName(ctx, baseURL, startPath)
	}
	saved := flags.Project
	flags.Project = refProjectName
	defer func() { flags.Project = saved }()
	return resolveProjectIDAndName(ctx, baseURL, startPath)
}

// hydrateRefWithQualified does a daemon GET to resolve the issue's short_id
// and project name, then populates QualifiedID and ShortID on ref. Used by
// destructive verbs whose X-Kata-Confirm header requires "<project>#<short_id>".
//
// includeDeleted matches the destructive flows that operate on soft-deleted
// issues (purge of a previously-deleted issue, restore).
func hydrateRefWithQualified(ctx context.Context, baseURL string, pid int64, ref resolvedIssueRef, includeDeleted bool) (resolvedIssueRef, error) {
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return ref, err
	}
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, pid, url.PathEscape(ref.RefForAPI))
	if includeDeleted {
		path += "?include_deleted=true"
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet, path, nil)
	if err != nil {
		return ref, err
	}
	if status >= 400 {
		return ref, apiErrFromBody(status, bs)
	}
	var out struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(bs, &out); err != nil {
		return ref, err
	}
	ref.ShortID = out.Issue.ShortID
	if ref.ProjectName != "" && ref.ShortID != "" {
		ref.QualifiedID = ref.ProjectName + "#" + ref.ShortID
	}
	return ref, nil
}

// workspaceProjectName reads .kata.toml at startPath and returns its project
// name. Returns "" when no readable .kata.toml is found or when it has no
// name binding; callers downstream treat that as "no workspace project" and
// require qualified refs.
func workspaceProjectName(startPath string) string {
	cfg, _, err := config.FindProjectConfig(startPath)
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.Project.Name
}

// resolveRefToWireOpts resolves one link-flag ref string to the wire value the
// daemon expects. Used by relationship flags on `kata edit` and `kata create`
// so they accept the same shapes as positional issue-ref args.
//
// flagName is folded into validation errors so the user knows which flag failed.
//
// currentProject is the canonical name of the project the surrounding command
// targets (i.e. the URL issue's project for `kata edit`, the create-time project
// for `kata create`). A qualified ref whose project segment differs from
// currentProject is forwarded verbatim as "project#short_id" — the daemon
// resolves link targets in the named project (storage v16, cross-project links).
// Bare refs and same-project qualified refs are forwarded as plain short_ids or
// ULIDs so the daemon resolves them against the URL issue's project. Pass "" to
// skip the qualified-forward path (intended for callers that can't yet plumb the
// canonical name).
//
// includeDeleted=true matches the soft-delete-tolerant lookup the daemon's
// remove paths use. The remove flags pass true; the add flags pass false.
func resolveRefToWireOpts(ctx context.Context, baseURL, currentProject string, projectID int64, ref, flagName string, includeDeleted bool) (string, error) {
	_ = ctx
	_ = baseURL
	_ = projectID
	_ = includeDeleted
	if strings.TrimSpace(ref) == "" {
		return "", &cliError{
			Message:  fmt.Sprintf("%s must not be empty", flagName),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	// Pre-flight: surface a legacy numeric ref with the documented error so
	// users see "use a short_id" instead of a daemon-side "issue not found".
	// Pass currentProject (when known) as the workspace fallback so bare
	// refs validate without requiring callers to plumb their own.
	fallbackProject := currentProject
	if fallbackProject == "" {
		// ResolveRef rejects numerics before consulting workspaceProject,
		// so a placeholder is safe when the caller doesn't yet plumb
		// the canonical name.
		fallbackProject = "anything"
	}
	parsed, err := ResolveRef(ref, fallbackProject)
	if err != nil {
		// Surface validation errors from ResolveRef (legacy numbers, bad
		// shortid syntax) with a flag-name prefix so the user can tell
		// which flag was malformed.
		return "", &cliError{
			Message:  fmt.Sprintf("%s: %s", flagName, err.Error()),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	// A qualified ref ("other#abc4") names a project explicitly. Links may
	// span projects (storage v16), and the daemon resolves link-target refs
	// in the named project — forward the qualified form verbatim so the
	// daemon can. Bare refs and same-project qualified refs keep the bare
	// wire shape the daemon resolves against the URL issue's project.
	// For ULID refs, ResolveRef sets ProjectName to the workspace fallback
	// (same as currentProject), so parsed.ProjectName == currentProject and
	// the condition below is false; ULID refs are forwarded bare in RefForAPI.
	if currentProject != "" && parsed.ProjectName != "" && parsed.ProjectName != currentProject {
		return parsed.ProjectName + "#" + parsed.RefForAPI, nil
	}
	return parsed.RefForAPI, nil
}

// resolveRefSliceToWire maps every entry of refs through resolveRefToWireOpts.
// Returns the slice in the original order. Empty refs is OK (nil out, nil err).
func resolveRefSliceToWire(ctx context.Context, baseURL, currentProject string, projectID int64, refs []string, flagName string) ([]string, error) {
	return resolveRefSliceToWireOpts(ctx, baseURL, currentProject, projectID, refs, flagName, false, false)
}

// resolveRefSliceToWireIdempotentRemove is the variant the
// idempotent --remove-blocks / --remove-blocked-by / --remove-related
// flags use. In addition to soft-delete tolerance, it drops refs that
// resolve to "issue not found" entirely.
func resolveRefSliceToWireIdempotentRemove(ctx context.Context, baseURL, currentProject string, projectID int64, refs []string, flagName string) ([]string, error) {
	return resolveRefSliceToWireOpts(ctx, baseURL, currentProject, projectID, refs, flagName, true, true)
}

func resolveRefSliceToWireOpts(ctx context.Context, baseURL, currentProject string, projectID int64, refs []string, flagName string, includeDeleted, tolerateNotFound bool) ([]string, error) {
	_ = tolerateNotFound // refs are validated locally; daemon does the existence check
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		s, err := resolveRefToWireOpts(ctx, baseURL, currentProject, projectID, r, flagName, includeDeleted)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// resolveSingletonRefToWire resolves the StringSliceVar storage for an
// at-most-one flag (--parent, --remove-parent) and returns the ref string
// every entry resolves to. Rejects only when entries resolve to *different*
// refs (after normalization), so equivalent forms — `abc4` and `kata#abc4`,
// the same string twice — succeed.
func resolveSingletonRefToWire(ctx context.Context, baseURL, currentProject string, projectID int64, values []string, flagName string, includeDeleted bool) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	first := strings.TrimSpace(values[0])
	firstResolved, err := resolveRefToWireOpts(ctx, baseURL, currentProject, projectID, first, flagName, includeDeleted)
	if err != nil {
		return "", err
	}
	for _, v := range values[1:] {
		trimmed := strings.TrimSpace(v)
		if trimmed == first {
			continue
		}
		resolved, err := resolveRefToWireOpts(ctx, baseURL, currentProject, projectID, trimmed, flagName, includeDeleted)
		if err != nil {
			return "", err
		}
		if resolved != firstResolved {
			return "", &cliError{
				Message: fmt.Sprintf("%s only accepts one ref; got %q and %q which resolve to different issues",
					flagName, first, trimmed),
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
	}
	return firstResolved, nil
}
