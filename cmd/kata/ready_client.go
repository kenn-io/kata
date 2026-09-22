package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type readyIssueForCLI struct {
	Raw         jsontext.Value `json:"-"`
	ProjectID   int64          `json:"project_id"`
	ProjectName string         `json:"project_name"`
	ShortID     string         `json:"short_id"`
	Title       string         `json:"title"`
	Owner       *string        `json:"owner,omitempty"`
	Priority    *int64         `json:"priority,omitzero"`
	Labels      []string       `json:"labels"`
}

type readyOptions struct {
	Limit    int
	All      bool
	Unowned  bool
	Owner    string
	Labels   []string
	NoLabels []string
}

type readyResultForCLI struct {
	Raw    jsontext.Value
	Issues []readyIssueForCLI
}

func (o readyOptions) validate() error {
	if o.Limit < 0 {
		return &cliError{Message: "--limit must be non-negative", Kind: kindValidation, ExitCode: ExitValidation}
	}
	if o.Unowned && o.Owner != "" {
		return &cliError{Message: "--unowned and --owner are mutually exclusive", Kind: kindValidation, ExitCode: ExitValidation}
	}
	if o.All && (strings.TrimSpace(flags.Project) != "" || strings.TrimSpace(flags.Workspace) != "") {
		return &cliError{
			Message:  "--all is mutually exclusive with --project and --workspace",
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	return nil
}

func (o readyOptions) fetch(cmd *cobra.Command) (readyResultForCLI, error) {
	if err := o.validate(); err != nil {
		return readyResultForCLI{}, err
	}

	ctx := cmd.Context()
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return readyResultForCLI{}, err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return readyResultForCLI{}, err
	}
	if o.All && (o.Unowned || o.Owner != "" || len(o.Labels) > 0 || len(o.NoLabels) > 0) {
		if err := requireDaemonAPIVersion(ctx, client, baseURL,
			apiVersionReadyAndSearchFilters, "filtered ready --all"); err != nil {
			return readyResultForCLI{}, err
		}
	}

	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return readyResultForCLI{}, err
	}
	params := &generated.ReadyIssuesQuery{Label: o.Labels, ExcludeLabel: o.NoLabels}
	if o.Limit > 0 {
		params.Limit = new(int64(o.Limit))
	}
	if o.Unowned {
		params.Unowned = &o.Unowned
	}
	if o.Owner != "" {
		params.Owner = &o.Owner
	}
	if o.All {
		response, callErr := apiClient.ReadyIssuesGlobalWithResponse(ctx, &generated.ReadyIssuesGlobalRequestOptions{
			Query: (*generated.ReadyIssuesGlobalQuery)(params),
		})
		if response == nil {
			return readyResultForCLI{}, externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return readyResultForCLI{}, err
		}
		return decodeReadyResult(response.Body)
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return readyResultForCLI{}, err
	}
	projectID, err := resolveProjectID(ctx, baseURL, start)
	if err != nil {
		return readyResultForCLI{}, err
	}
	response, callErr := apiClient.ReadyIssuesWithResponse(ctx, &generated.ReadyIssuesRequestOptions{
		PathParams: &generated.ReadyIssuesPath{ProjectID: projectID}, Query: params,
	})
	if response == nil {
		return readyResultForCLI{}, externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return readyResultForCLI{}, err
	}
	return decodeReadyResult(response.Body)
}

func decodeReadyResult(body []byte) (readyResultForCLI, error) {
	var envelope struct {
		Issues []jsontext.Value `json:"issues"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return readyResultForCLI{}, err
	}

	result := readyResultForCLI{
		Raw:    jsontext.Value(body),
		Issues: make([]readyIssueForCLI, 0, len(envelope.Issues)),
	}
	for _, raw := range envelope.Issues {
		var issue readyIssueForCLI
		if err := json.Unmarshal(raw, &issue); err != nil {
			return readyResultForCLI{}, err
		}
		issue.Raw = raw
		result.Issues = append(result.Issues, issue)
	}
	return result, nil
}

func selectNextReadyIssue(candidates []readyIssueForCLI) (readyIssueForCLI, bool) {
	if len(candidates) == 0 {
		return readyIssueForCLI{}, false
	}

	selected := candidates[0]
	for _, candidate := range candidates[1:] {
		switch {
		case selected.Priority == nil && candidate.Priority != nil:
			selected = candidate
		case selected.Priority != nil && candidate.Priority != nil && *candidate.Priority < *selected.Priority:
			selected = candidate
		}
	}
	return selected, true
}
