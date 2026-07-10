package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type readyIssueForCLI struct {
	Raw         json.RawMessage `json:"-"`
	ProjectID   int64           `json:"project_id"`
	ProjectName string          `json:"project_name"`
	ShortID     string          `json:"short_id"`
	Title       string          `json:"title"`
	Owner       *string         `json:"owner,omitempty"`
	Priority    *int64          `json:"priority,omitempty"`
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
	Raw    json.RawMessage
	Issues []readyIssueForCLI
}

func (o readyOptions) validate() error {
	if o.Limit < 0 {
		return &cliError{Message: "--limit must be non-negative", Kind: kindValidation, ExitCode: ExitValidation}
	}
	if o.Unowned && o.Owner != "" {
		return &cliError{Message: "--unowned and --owner are mutually exclusive", Kind: kindValidation, ExitCode: ExitValidation}
	}
	if o.All && strings.TrimSpace(flags.Project) != "" {
		return &cliError{
			Message:  "--project and --all are mutually exclusive",
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	if o.All && (o.Unowned || o.Owner != "" || len(o.Labels) > 0 || len(o.NoLabels) > 0) {
		return &cliError{
			Message:  "--all does not support --unowned, --owner, --label, or --no-label",
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	return nil
}

func (o readyOptions) query() url.Values {
	params := url.Values{}
	if o.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", o.Limit))
	}
	if o.Unowned {
		params.Set("unowned", "true")
	}
	if o.Owner != "" {
		params.Set("owner", o.Owner)
	}
	for _, label := range o.Labels {
		params.Add("label", label)
	}
	for _, label := range o.NoLabels {
		params.Add("exclude_label", label)
	}
	return params
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

	getURL, err := o.endpoint(ctx, baseURL)
	if err != nil {
		return readyResultForCLI{}, err
	}
	if params := o.query(); len(params) > 0 {
		getURL += "?" + params.Encode()
	}

	status, body, err := httpDoJSON(ctx, client, http.MethodGet, getURL, nil)
	if err != nil {
		return readyResultForCLI{}, err
	}
	if status >= 400 {
		return readyResultForCLI{}, apiErrFromBody(status, body)
	}
	return decodeReadyResult(body)
}

func (o readyOptions) endpoint(ctx context.Context, baseURL string) (string, error) {
	if o.All {
		return baseURL + "/api/v1/ready", nil
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return "", err
	}
	projectID, err := resolveProjectID(ctx, baseURL, start)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/api/v1/projects/%d/ready", baseURL, projectID), nil
}

func decodeReadyResult(body []byte) (readyResultForCLI, error) {
	var envelope struct {
		Issues []json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return readyResultForCLI{}, err
	}

	result := readyResultForCLI{
		Raw:    json.RawMessage(body),
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
