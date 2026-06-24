package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/textsafe"
)

type githubSyncOptions struct {
	repo        string
	host        string
	interval    string
	titlePrefix bool
}

type githubSyncBindingBody struct {
	Binding *githubSyncBindingOut `json:"binding"`
	Status  githubSyncStatusOut   `json:"status"`
}

type githubSyncOnceBody struct {
	Binding *githubSyncBindingOut `json:"binding"`
	Status  githubSyncStatusOut   `json:"status"`
	Import  struct {
		Created   int `json:"created"`
		Updated   int `json:"updated"`
		Unchanged int `json:"unchanged"`
		Comments  int `json:"comments"`
		Links     int `json:"links"`
	} `json:"import"`
}

type githubSyncBindingOut struct {
	ID              int64          `json:"id"`
	ProjectID       int64          `json:"project_id"`
	Provider        string         `json:"provider"`
	SourceKey       string         `json:"source_key"`
	RemoteID        string         `json:"remote_id"`
	DisplayName     string         `json:"display_name"`
	Config          map[string]any `json:"config"`
	Enabled         bool           `json:"enabled"`
	IntervalSeconds int            `json:"interval_seconds"`
}

type githubSyncStatusOut struct {
	BindingID     int64  `json:"binding_id"`
	ProjectID     int64  `json:"project_id"`
	Provider      string `json:"provider"`
	Enabled       bool   `json:"enabled"`
	State         string `json:"state"`
	LastError     string `json:"last_error"`
	LastCreated   int    `json:"last_created"`
	LastUpdated   int    `json:"last_updated"`
	LastUnchanged int    `json:"last_unchanged"`
	LastComments  int    `json:"last_comments"`
}

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "sync external systems",
	}
	cmd.AddCommand(newGitHubSyncCmd())
	return cmd
}

func newGitHubSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "sync project issues with GitHub",
	}
	cmd.AddCommand(
		newGitHubSyncEnableCmd(),
		newGitHubSyncDisableCmd(),
		newIssueSyncStatusCmd(),
		newGitHubSyncOnceCmd(),
	)
	return cmd
}

func newGitHubSyncEnableCmd() *cobra.Command {
	var opts githubSyncOptions
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "enable GitHub sync for this project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			client, baseURL, projectID, err := githubSyncProjectClient(ctx)
			if err != nil {
				return err
			}
			binding, err := githubSyncResolveBinding(ctx, client, baseURL, projectID, opts)
			if err != nil {
				return err
			}
			body := map[string]any{
				"config": map[string]any{
					"host":         binding.Host,
					"owner":        binding.Owner,
					"repo":         binding.Repo,
					"title_prefix": opts.titlePrefix,
				},
			}
			if strings.TrimSpace(opts.interval) != "" {
				body["interval"] = strings.TrimSpace(opts.interval)
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				issueSyncEndpointURL(baseURL, projectID, "github", "enable"), body)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return githubSyncPrintBindingBody(cmd.OutOrStdout(), bs, "enabled")
		},
	}
	cmd.Flags().StringVar(&opts.repo, "repo", "", "GitHub repository as owner/repo")
	cmd.Flags().StringVar(&opts.host, "host", "", "GitHub host (default: github.com)")
	cmd.Flags().StringVar(&opts.interval, "interval", "", "sync interval duration, such as 5m")
	cmd.Flags().BoolVar(&opts.titlePrefix, "title-prefix", true, "prefix imported issue titles with [GitHub #N]")
	return cmd
}

func newGitHubSyncDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "disable GitHub sync for this project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return githubSyncPostEmpty(cmd, "disable", "disabled")
		},
	}
}

func newIssueSyncStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show GitHub sync status for this project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			client, baseURL, projectID, err := githubSyncProjectClient(ctx)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet,
				issueSyncEndpointURL(baseURL, projectID, "github", "status"), nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return githubSyncPrintBindingBody(cmd.OutOrStdout(), bs, "status")
		},
	}
}

func newGitHubSyncOnceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "once",
		Short: "run GitHub sync once for this project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, baseURL, projectID, err := githubSyncProjectClient(ctx)
			if err != nil {
				return err
			}
			client, err := longRunningClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				issueSyncEndpointURL(baseURL, projectID, "github", "once"), map[string]any{})
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return githubSyncPrintOnceBody(cmd.OutOrStdout(), bs)
		},
	}
}

func githubSyncPostEmpty(cmd *cobra.Command, endpoint, action string) error {
	ctx := cmd.Context()
	client, baseURL, projectID, err := githubSyncProjectClient(ctx)
	if err != nil {
		return err
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
		issueSyncEndpointURL(baseURL, projectID, "github", endpoint), map[string]any{})
	if err != nil {
		return err
	}
	if status >= 400 {
		return apiErrFromBody(status, bs)
	}
	return githubSyncPrintBindingBody(cmd.OutOrStdout(), bs, action)
}

func issueSyncEndpointURL(baseURL string, projectID int64, provider, action string) string {
	return fmt.Sprintf("%s/api/v1/projects/%d/issue-sync/%s/%s", baseURL, projectID, provider, action)
}

func githubSyncProjectClient(ctx context.Context) (*http.Client, string, int64, error) {
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return nil, "", 0, err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return nil, "", 0, err
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return nil, "", 0, err
	}
	projectID, _, err := resolveProjectIDAndNameWithClient(ctx, client, baseURL, start)
	if err != nil {
		return nil, "", 0, err
	}
	return client, baseURL, projectID, nil
}

func githubSyncResolveBinding(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	projectID int64,
	opts githubSyncOptions,
) (githubsync.Binding, error) {
	if strings.TrimSpace(opts.repo) != "" {
		owner, repo, err := parseGitHubSyncRepo(opts.repo)
		if err != nil {
			return githubsync.Binding{}, err
		}
		host := strings.ToLower(strings.TrimSpace(opts.host))
		if host == "" {
			host = "github.com"
		}
		return githubsync.Binding{Host: host, Owner: owner, Repo: repo}, nil
	}
	return inferIssueSyncBinding(ctx, client, baseURL, projectID, opts.host)
}

func parseGitHubSyncRepo(repo string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", &cliError{
			Message:  "--repo must be owner/repo",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func inferIssueSyncBinding(ctx context.Context, client *http.Client, baseURL string, projectID int64, requestedHost string) (githubsync.Binding, error) {
	requestedHost = strings.ToLower(strings.TrimSpace(requestedHost))
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d", baseURL, projectID), nil)
	if err != nil {
		return githubsync.Binding{}, err
	}
	if status >= 400 {
		return githubsync.Binding{}, apiErrFromBody(status, bs)
	}
	var out struct {
		Aliases []projectAliasRef `json:"aliases"`
	}
	if err := json.Unmarshal(bs, &out); err != nil {
		return githubsync.Binding{}, err
	}
	matches := make([]githubsync.Binding, 0, 1)
	seen := map[string]bool{}
	for _, alias := range out.Aliases {
		if alias.AliasKind != "git" {
			continue
		}
		binding, err := githubsync.ParseGitHubRemote(alias.AliasIdentity)
		if err != nil {
			continue
		}
		if requestedHost != "" && binding.Host != requestedHost {
			continue
		}
		key := binding.Host + "/" + binding.Owner + "/" + binding.Repo
		if seen[key] {
			continue
		}
		seen[key] = true
		matches = append(matches, binding)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return githubsync.Binding{}, &cliError{
			Message:  "could not infer GitHub repository; pass --repo owner/repo",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	default:
		return githubsync.Binding{}, &cliError{
			Message:  "ambiguous GitHub repository aliases; pass --repo owner/repo",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
}

func githubSyncPrintBindingBody(w io.Writer, bs []byte, action string) error {
	switch currentOutputMode() {
	case outputJSON:
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(w, buf.String())
		return err
	}
	var body githubSyncBindingBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		return githubSyncPrintAgent(w, action, body.Status, body.Binding)
	}
	return githubSyncPrintHumanBinding(w, action, body)
}

func githubSyncPrintOnceBody(w io.Writer, bs []byte) error {
	switch currentOutputMode() {
	case outputJSON:
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(w, buf.String())
		return err
	}
	var body githubSyncOnceBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		if err := githubSyncPrintAgent(w, "once", body.Status, body.Binding); err != nil {
			return err
		}
		return nil
	}
	_, err := fmt.Fprintf(w,
		"GitHub sync ran: created=%d updated=%d unchanged=%d comments=%d links=%d\n",
		body.Import.Created, body.Import.Updated, body.Import.Unchanged, body.Import.Comments, body.Import.Links)
	return err
}

func githubSyncPrintAgent(w io.Writer, action string, status githubSyncStatusOut, binding *githubSyncBindingOut) error {
	if _, err := fmt.Fprintf(w, "OK github-sync action=%s state=%s enabled=%s",
		agentValue(action), agentValue(githubSyncState(status, binding)), strconv.FormatBool(githubSyncEnabled(status, binding))); err != nil {
		return err
	}
	if binding != nil {
		if _, err := fmt.Fprintf(w, " repo=%s", agentValue(githubSyncRepoLabel(binding))); err != nil {
			return err
		}
	}
	if status.LastError != "" {
		if _, err := fmt.Fprintf(w, " last_error=%s", agentValue(status.LastError)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func githubSyncPrintHumanBinding(w io.Writer, action string, body githubSyncBindingBody) error {
	switch action {
	case "enabled":
		if body.Binding == nil {
			_, err := fmt.Fprintln(w, "GitHub sync enabled")
			return err
		}
		if _, err := fmt.Fprintf(w, "GitHub sync enabled for %s",
			textsafe.Line(githubSyncRepoLabel(body.Binding))); err != nil {
			return err
		}
		if body.Binding.IntervalSeconds > 0 {
			if _, err := fmt.Fprintf(w, " every %ds", body.Binding.IntervalSeconds); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	case "disabled":
		if _, err := fmt.Fprintln(w, "GitHub sync disabled"); err != nil {
			return err
		}
	default:
		state := githubSyncState(body.Status, body.Binding)
		if _, err := fmt.Fprintf(w, "GitHub sync %s\n", textsafe.Line(state)); err != nil {
			return err
		}
	}
	if body.Status.LastError != "" {
		_, err := fmt.Fprintf(w, "Last error: %s\n", textsafe.Line(body.Status.LastError))
		return err
	}
	return nil
}

func githubSyncRepoLabel(binding *githubSyncBindingOut) string {
	if binding == nil {
		return ""
	}
	host := githubSyncConfigString(binding.Config, "host")
	owner := githubSyncConfigString(binding.Config, "owner")
	repo := githubSyncConfigString(binding.Config, "repo")
	if host != "" && owner != "" && repo != "" {
		return host + "/" + owner + "/" + repo
	}
	if binding.DisplayName != "" {
		return binding.DisplayName
	}
	return binding.Provider + "/" + binding.RemoteID
}

func githubSyncConfigString(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	if value, ok := config[key].(string); ok {
		return value
	}
	return ""
}

func githubSyncState(status githubSyncStatusOut, binding *githubSyncBindingOut) string {
	if status.State != "" {
		return status.State
	}
	if githubSyncEnabled(status, binding) {
		return "enabled"
	}
	return "disabled"
}

func githubSyncEnabled(status githubSyncStatusOut, binding *githubSyncBindingOut) bool {
	if binding != nil {
		return binding.Enabled
	}
	return status.Enabled
}
