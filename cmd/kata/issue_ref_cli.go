package main

import (
	"context"
	"encoding/json"
	"errors"
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
//
// Destructive verbs that operate on soft-deleted issues get that tolerance
// from their subsequent hydrateRefWithQualified call with includeDeleted=true;
// resolving the positional ref itself needs no destructive-command option.
func resolveIssueRefForCommand(cmd *cobra.Command, ref string) (context.Context, string, int64, resolvedIssueRef, error) {
	return resolveIssueRefForCommandWithOptions(cmd, ref, false)
}

func resolveIssueRefForCommandWithOptions(
	cmd *cobra.Command,
	ref string,
	includeArchivedProject bool,
) (context.Context, string, int64, resolvedIssueRef, error) {
	ctx := cmd.Context()
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return nil, "", 0, resolvedIssueRef{}, err
	}
	a, err := dialDaemon(ctx)
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
	pid, projectName, err := resolveProjectIDAndNameForRef(
		a, start, parsed.ProjectName, includeArchivedProject,
	)
	if err != nil {
		return nil, "", 0, resolvedIssueRef{}, err
	}
	return ctx, a.baseURL, pid, resolvedIssueRef{
		RefForAPI:   parsed.RefForAPI,
		ProjectName: projectName,
	}, nil
}

// resolveProjectIDAndNameForRef resolves the project ID + canonical project
// name needed by ref-consuming commands. A qualified ref (e.g. "kata#abc4")
// pins the project name; a bare ref / ULID inherits the workspace binding.
// The canonical name is used by destructive verbs to format the
// X-Kata-Confirm header value ("DELETE <project>#<short_id>").
func resolveProjectIDAndNameForRef(
	a daemonAPI,
	startPath, refProjectName string,
	includeArchived bool,
) (int64, string, error) {
	if strings.TrimSpace(refProjectName) == "" {
		return resolveProjectIDAndNameWithClient(a, startPath)
	}
	saved := flags.Project
	flags.Project = refProjectName
	defer func() { flags.Project = saved }()
	projectID, projectName, err := resolveProjectIDAndNameWithClient(a, startPath)
	if err == nil || !includeArchived {
		return projectID, projectName, err
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.Kind != kindNotFound {
		return 0, "", err
	}
	project, err := resolveProjectSelectorIncludingArchived(a, refProjectName)
	if err != nil {
		return 0, "", err
	}
	return project.ID, project.Name, nil
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

// refToWire purely parses one link-flag ref into the wire value the daemon
// expects. The daemon owns existence, soft-delete, and idempotent-remove checks.
//
// When currentProject is known, a qualified ref for another project stays
// qualified; otherwise refs use the bare wire shape.
func refToWire(ref, flagName, currentProject string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", &cliError{
			Message:  fmt.Sprintf("%s must not be empty", flagName),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	fallbackProject := currentProject
	if fallbackProject == "" {
		fallbackProject = "anything"
	}
	parsed, err := ResolveRef(ref, fallbackProject)
	if err != nil {
		return "", &cliError{
			Message:  fmt.Sprintf("%s: %s", flagName, err.Error()),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	if currentProject != "" && parsed.ProjectName != "" && parsed.ProjectName != currentProject {
		return parsed.ProjectName + "#" + parsed.RefForAPI, nil
	}
	return parsed.RefForAPI, nil
}

// refsToWire preserves the input order and rejects the whole slice when any
// entry is invalid.
func refsToWire(refs []string, flagName, currentProject string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		s, err := refToWire(r, flagName, currentProject)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// singletonRefToWire normalizes an at-most-one flag's StringSliceVar storage.
// Equivalent repeated forms are accepted; distinct refs are rejected.
func singletonRefToWire(values []string, flagName, currentProject string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	first := strings.TrimSpace(values[0])
	firstResolved, err := refToWire(first, flagName, currentProject)
	if err != nil {
		return "", err
	}
	for _, v := range values[1:] {
		trimmed := strings.TrimSpace(v)
		if trimmed == first {
			continue
		}
		resolved, err := refToWire(trimmed, flagName, currentProject)
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
