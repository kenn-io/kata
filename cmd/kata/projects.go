package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/textsafe"
)

type projectAliasRef struct {
	ID            int64  `json:"id"`
	ProjectID     int64  `json:"project_id"`
	AliasIdentity string `json:"alias_identity"`
	AliasKind     string `json:"alias_kind"`
	RootPath      string `json:"root_path"`
}

type projectRef struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	DeletedAt *time.Time        `json:"deleted_at,omitempty"`
	Aliases   []projectAliasRef `json:"aliases,omitempty"`
}

// shortIDExtensionItem mirrors api.MergeShortIDExtension for decoding the
// merge response. One entry per source-side issue whose short_id was
// auto-extended during the merge to break a target-side collision
// (spec §9.4).
type shortIDExtensionItem struct {
	UID              string `json:"uid"`
	PreMergeShortID  string `json:"pre_merge_short_id"`
	PostMergeShortID string `json:"post_merge_short_id"`
}

func newProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "projects", Short: "list and inspect kata projects"}
	cmd.AddCommand(projectsListCmd(), projectsShowCmd(), projectsRenameCmd(),
		projectsMergeCmd(), projectsRemoveCmd(), projectsRestoreCmd(), projectsDetachCmd())
	return cmd
}

func projectsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list known projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			mode := currentOutputMode()
			path := baseURL + "/api/v1/projects"
			if mode == outputAgent {
				path += "?include=stats"
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Projects []struct {
					ID    int64  `json:"id"`
					Name  string `json:"name"`
					Stats *struct {
						Open   int `json:"open"`
						Closed int `json:"closed"`
					} `json:"stats"`
				} `json:"projects"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if mode == outputAgent {
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "OK projects count=%d\n", len(b.Projects)); err != nil {
					return err
				}
				for _, p := range b.Projects {
					open, closed := 0, 0
					if p.Stats != nil {
						open = p.Stats.Open
						closed = p.Stats.Closed
					}
					if err := writeAgentKVRow(out,
						agentRowField("project", p.Name),
						agentRowField("id", fmt.Sprint(p.ID)),
						agentRowField("open", fmt.Sprint(open)),
						agentRowField("closed", fmt.Sprint(closed)),
					); err != nil {
						return err
					}
				}
				return nil
			}
			for _, p := range b.Projects {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d  %s\n",
					p.ID, textsafe.Line(p.Name)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func projectsRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <project> <name>",
		Short: "rename a project",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(strings.Join(args[1:], " "))
			if name == "" {
				return &cliError{Message: "project name must be non-empty", Kind: kindValidation, ExitCode: ExitValidation}
			}

			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			project, err := resolveProjectSelector(ctx, client, baseURL, args[0])
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPatch,
				fmt.Sprintf("%s/api/v1/projects/%d", baseURL, project.ID),
				map[string]string{"name": name})
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Project struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"project"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if currentOutputMode() == outputAgent {
				return writeAgentProjectAction(cmd.OutOrStdout(), "rename",
					agentRowField("id", fmt.Sprint(b.Project.ID)),
					agentRowField("project", b.Project.Name),
				)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "renamed project #%d to %s\n",
				b.Project.ID, textsafe.Line(b.Project.Name))
			return err
		},
	}
}

func projectsMergeCmd() *cobra.Command {
	var targetName string
	cmd := &cobra.Command{
		Use:   "merge <source> <target>",
		Short: "merge one project into a surviving project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			source, err := resolveProjectSelector(ctx, client, baseURL, args[0])
			if err != nil {
				return err
			}
			target, err := resolveProjectSelector(ctx, client, baseURL, args[1])
			if err != nil {
				return err
			}
			if source.ID == target.ID {
				return &cliError{
					Message:  "source and target project must be different",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			body := map[string]any{"source_project_id": source.ID}
			if strings.TrimSpace(targetName) != "" {
				body["target_name"] = strings.TrimSpace(targetName)
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				fmt.Sprintf("%s/api/v1/projects/%d/merge", baseURL, target.ID), body)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			var b struct {
				Source            projectRef             `json:"source"`
				Target            projectRef             `json:"target"`
				IssuesMoved       int64                  `json:"issues_moved"`
				AliasesMoved      int64                  `json:"aliases_moved"`
				EventsMoved       int64                  `json:"events_moved"`
				ShortIDExtensions []shortIDExtensionItem `json:"short_id_extensions,omitempty"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if err := repairMergedWorkspaceBinding(source, b.Target); err != nil {
				return err
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			if currentOutputMode() == outputAgent {
				if err := writeAgentProjectAction(cmd.OutOrStdout(), "merge",
					agentRowField("source", fmt.Sprint(b.Source.ID)),
					agentRowField("target", fmt.Sprint(b.Target.ID)),
					agentRowField("project", b.Target.Name),
					agentRowField("issues_moved", fmt.Sprint(b.IssuesMoved)),
					agentRowField("aliases_moved", fmt.Sprint(b.AliasesMoved)),
					agentRowField("events_moved", fmt.Sprint(b.EventsMoved)),
				); err != nil {
					return err
				}
				for _, ext := range b.ShortIDExtensions {
					if err := writeAgentKVRow(cmd.OutOrStdout(),
						agentRowField("uid", ext.UID),
						agentRowField("pre_merge_short_id", ext.PreMergeShortID),
						agentRowField("post_merge_short_id", ext.PostMergeShortID),
					); err != nil {
						return err
					}
				}
				return nil
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(),
				"merged project #%d into #%d (%s); moved %s, %s, %s\n",
				b.Source.ID, b.Target.ID, textsafe.Line(b.Target.Name),
				pluralCount(b.IssuesMoved, "issue"),
				pluralCount(b.AliasesMoved, "alias"),
				pluralCount(b.EventsMoved, "event"),
			); err != nil {
				return err
			}
			for _, ext := range b.ShortIDExtensions {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(),
					"extended %s#%s from %s to %s\n",
					textsafe.Line(b.Target.Name), ext.PostMergeShortID,
					ext.PreMergeShortID, ext.PostMergeShortID); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&targetName, "rename-target", "", "rename the surviving target project after merge")
	return cmd
}

func repairMergedWorkspaceBinding(source, target projectRef) error {
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return err
	}
	disc, err := config.DiscoverPaths(start)
	if err != nil {
		return err
	}
	if disc.WorkspaceRoot == "" {
		return nil
	}
	cfg, err := config.ReadProjectConfig(disc.WorkspaceRoot)
	if err != nil {
		if errors.Is(err, config.ErrProjectConfigMissing) {
			return nil
		}
		return err
	}
	if !projectConfigReferencesSource(cfg.Project.Name, source) {
		return nil
	}
	if err := config.WriteProjectConfig(disc.WorkspaceRoot, target.Name); err != nil {
		return fmt.Errorf("repair .kata.toml after project merge: %w", err)
	}
	return nil
}

func projectConfigReferencesSource(name string, source projectRef) bool {
	if name == source.Name {
		return true
	}
	for _, alias := range source.Aliases {
		if name == alias.AliasIdentity {
			return true
		}
	}
	return false
}

func projectsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <project>",
		Short: "show project details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			project := projectRef{}
			if id, parseErr := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64); parseErr == nil {
				project.ID = id
			} else {
				var err error
				project, err = resolveProjectSelector(ctx, client, baseURL, args[0])
				if err != nil {
					return err
				}
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet,
				fmt.Sprintf("%s/api/v1/projects/%d", baseURL, project.ID), nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Project struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"project"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if currentOutputMode() == outputAgent {
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintln(out, "OK projects count=1"); err != nil {
					return err
				}
				return writeAgentKVRow(out,
					agentRowField("project", b.Project.Name),
					agentRowField("id", fmt.Sprint(b.Project.ID)),
				)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "#%d %s\n",
				b.Project.ID, textsafe.Line(b.Project.Name))
			return err
		},
	}
}

func resolveProjectSelector(ctx context.Context, client *http.Client, baseURL, selector string) (projectRef, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return projectRef{}, &cliError{Message: "project selector must be non-empty", Kind: kindValidation, ExitCode: ExitValidation}
	}
	projects, err := loadProjectRefs(ctx, client, baseURL)
	if err != nil {
		return projectRef{}, err
	}
	return resolveProjectSelectorFromRefs(selector, projects)
}

func resolveProjectSelectorIncludingArchived(ctx context.Context, client *http.Client, baseURL, selector string) (projectRef, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return projectRef{}, &cliError{Message: "project selector must be non-empty", Kind: kindValidation, ExitCode: ExitValidation}
	}
	projects, err := loadProjectRefsIncludingArchived(ctx, client, baseURL)
	if err != nil {
		return projectRef{}, err
	}
	return resolveProjectSelectorFromRefs(selector, projects)
}

func resolveProjectSelectorFromRefs(selector string, projects []projectRef) (projectRef, error) {
	if id, parseErr := strconv.ParseInt(selector, 10, 64); parseErr == nil {
		for _, project := range projects {
			if project.ID == id {
				return project, nil
			}
		}
		return projectRef{}, &cliError{
			Message:  fmt.Sprintf("project #%d not found", id),
			Kind:     kindNotFound,
			ExitCode: ExitNotFound,
		}
	}
	if match, ok, err := uniqueProjectMatch(selector, projects, projectMatchesSelector); ok || err != nil {
		return match, err
	}
	return projectRef{}, &cliError{
		Message:  fmt.Sprintf("project selector %q did not match any project", selector),
		Kind:     kindNotFound,
		ExitCode: ExitNotFound,
	}
}

func loadProjectRefs(ctx context.Context, client *http.Client, baseURL string) ([]projectRef, error) {
	return loadProjectRefsWithArchived(ctx, client, baseURL, false)
}

func loadProjectRefsIncludingArchived(ctx context.Context, client *http.Client, baseURL string) ([]projectRef, error) {
	return loadProjectRefsWithArchived(ctx, client, baseURL, true)
}

func loadProjectRefsWithArchived(ctx context.Context, client *http.Client, baseURL string, includeArchived bool) ([]projectRef, error) {
	path := baseURL + "/api/v1/projects"
	if includeArchived {
		path += "?include=archived"
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, apiErrFromBody(status, bs)
	}
	var list struct {
		Projects []projectRef `json:"projects"`
	}
	if err := json.Unmarshal(bs, &list); err != nil {
		return nil, err
	}
	for i := range list.Projects {
		if list.Projects[i].DeletedAt != nil {
			continue
		}
		status, detail, err := httpDoJSON(ctx, client, http.MethodGet,
			fmt.Sprintf("%s/api/v1/projects/%d", baseURL, list.Projects[i].ID), nil)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, apiErrFromBody(status, detail)
		}
		var show struct {
			Aliases []projectAliasRef `json:"aliases"`
		}
		if err := json.Unmarshal(detail, &show); err != nil {
			return nil, err
		}
		list.Projects[i].Aliases = show.Aliases
	}
	return list.Projects, nil
}

type projectSelectorMatcher func(projectRef, string) bool

func uniqueProjectMatch(selector string, projects []projectRef, matchesSelector projectSelectorMatcher) (projectRef, bool, error) {
	matches := make([]projectRef, 0, 1)
	seen := make(map[int64]bool)
	for _, project := range projects {
		if matchesSelector(project, selector) && !seen[project.ID] {
			seen[project.ID] = true
			matches = append(matches, project)
		}
	}
	switch len(matches) {
	case 0:
		return projectRef{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return projectRef{}, false, &cliError{
			Message:  ambiguousProjectSelectorMessage(selector, matches),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
}

func projectMatchesSelector(project projectRef, selector string) bool {
	return projectNameExact(project, selector) ||
		projectAliasExact(project, selector) ||
		projectNameSuffix(project, selector) ||
		projectAliasSuffix(project, selector)
}

func projectAliasExact(project projectRef, selector string) bool {
	for _, alias := range project.Aliases {
		if alias.AliasIdentity == selector {
			return true
		}
	}
	return false
}

func projectAliasSuffix(project projectRef, selector string) bool {
	for _, alias := range project.Aliases {
		if suffixMatches(alias.AliasIdentity, selector) {
			return true
		}
	}
	return false
}

func projectNameExact(project projectRef, selector string) bool {
	return project.Name == selector
}

func projectNameSuffix(project projectRef, selector string) bool {
	return suffixMatches(project.Name, selector)
}

func suffixMatches(value, selector string) bool {
	return value == selector ||
		strings.HasSuffix(value, "/"+selector) ||
		strings.HasSuffix(value, ":"+selector)
}

func ambiguousProjectSelectorMessage(selector string, matches []projectRef) string {
	parts := make([]string, 0, len(matches))
	for _, match := range matches {
		parts = append(parts, fmt.Sprintf("#%d %s", match.ID, textsafe.Line(match.Name)))
	}
	return fmt.Sprintf("project selector %q is ambiguous: %s", selector, strings.Join(parts, ", "))
}

func pluralCount(n int64, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	if strings.HasSuffix(noun, "s") {
		return fmt.Sprintf("%d %ses", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// projectsRemoveCmd archives a project: DELETE /api/v1/projects/{id}. The
// row stays so events keep a valid FK target, but list/resolve hide it and
// re-init against the same name returns project_archived. --force
// overrides the open-issue refusal.
func projectsRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <project>",
		Short: "archive a project (hides it from kata; events stay for audit)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			project, err := resolveProjectSelector(ctx, client, baseURL, args[0])
			if err != nil {
				return err
			}
			actor, _ := resolveActor(flags.As, nil)
			path := fmt.Sprintf("%s/api/v1/projects/%d?actor=%s",
				baseURL, project.ID, url.QueryEscape(actor))
			if force {
				path += "&force=true"
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodDelete, path, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			if currentOutputMode() == outputAgent {
				return writeAgentProjectAction(cmd.OutOrStdout(), "remove",
					agentRowField("id", fmt.Sprint(project.ID)),
					agentRowField("project", project.Name),
				)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"archived project #%d (%s); events preserved, aliases dropped\n",
				project.ID, textsafe.Line(project.Name))
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "archive even when open issues remain")
	return cmd
}

func projectsRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <project>",
		Short: "restore an archived project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			project, err := resolveProjectSelectorIncludingArchived(ctx, client, baseURL, args[0])
			if err != nil {
				return err
			}
			actor, _ := resolveActor(flags.As, nil)
			path := fmt.Sprintf("%s/api/v1/projects/%d/restore?actor=%s",
				baseURL, project.ID, url.QueryEscape(actor))
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost, path, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Project struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"project"`
				Changed bool `json:"changed"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if currentOutputMode() == outputAgent {
				return writeAgentProjectAction(cmd.OutOrStdout(), "restore",
					agentRowField("id", fmt.Sprint(b.Project.ID)),
					agentRowField("project", b.Project.Name),
					agentRowField("changed", strconv.FormatBool(b.Changed)),
				)
			}
			if !b.Changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(),
					"project #%d (%s) is already active\n",
					b.Project.ID, textsafe.Line(b.Project.Name))
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"restored project #%d (%s); run 'kata init' in the workspace to reattach an alias\n",
				b.Project.ID, textsafe.Line(b.Project.Name))
			return err
		},
	}
}

// projectsDetachCmd drops a single alias from a project: DELETE
// /api/v1/projects/{id}/aliases/{alias_id}. The selector is the alias_identity
// (e.g. github.com/foo/bar or local:///abs/path). --force overrides the
// last-alias refusal.
func projectsDetachCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "detach <alias-identity>",
		Short: "remove a single project alias",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			selector := strings.TrimSpace(args[0])
			projects, err := loadProjectRefs(ctx, client, baseURL)
			if err != nil {
				return err
			}
			projectID, aliasID, ok := findAliasBySelector(projects, selector)
			if !ok {
				return &cliError{
					Message:  fmt.Sprintf("alias %q did not match any project_aliases row", selector),
					Kind:     kindNotFound,
					ExitCode: ExitNotFound,
				}
			}
			actor, _ := resolveActor(flags.As, nil)
			path := fmt.Sprintf("%s/api/v1/projects/%d/aliases/%d?actor=%s",
				baseURL, projectID, aliasID, url.QueryEscape(actor))
			if force {
				path += "&force=true"
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodDelete, path, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			if currentOutputMode() == outputAgent {
				return writeAgentProjectAction(cmd.OutOrStdout(), "detach",
					agentRowField("project_id", fmt.Sprint(projectID)),
					agentRowField("alias_id", fmt.Sprint(aliasID)),
					agentRowField("alias", selector),
				)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"detached alias %q from project #%d\n", selector, projectID)
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "drop the alias even when it's the only one for its project")
	return cmd
}

func writeAgentProjectAction(w io.Writer, action string, fields ...agentField) error {
	if _, err := fmt.Fprintf(w, "OK project action=%s", agentValue(action)); err != nil {
		return err
	}
	for _, f := range fields {
		if f.value == nil {
			continue
		}
		if _, err := fmt.Fprintf(w, " %s=%s", f.name, agentValue(*f.value)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// findAliasBySelector matches an alias_identity exactly across every
// project's pre-loaded aliases. Returns the project_id and alias_id when one
// match exists.
func findAliasBySelector(projects []projectRef, selector string) (int64, int64, bool) {
	for _, project := range projects {
		for _, alias := range project.Aliases {
			if alias.AliasIdentity == selector {
				return project.ID, alias.ID, true
			}
		}
	}
	return 0, 0, false
}
