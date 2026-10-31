package tui

import (
	"context"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronAPI interface {
	ListCronJobs(context.Context, int64) ([]generated.CronJob, error)
	ListCronWorkflows(context.Context, int64) ([]generated.CronWorkflow, error)
	ListCronRuns(context.Context, int64) ([]generated.CronRun, error)
	readCronParent(ctx context.Context, projectID int64, parentUID string) (cronParent, error)
}

// ListCronJobs reads the selected project's current job definitions.
func (c *Client) ListCronJobs(ctx context.Context, projectID int64) ([]generated.CronJob, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := client.ListCronJobsWithResponse(ctx, &generated.ListCronJobsRequestOptions{
		PathParams: &generated.ListCronJobsPath{ProjectID: projectID},
	})
	if wire == nil {
		return nil, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, err
	}
	return wire.JSON200.Jobs, nil
}

// ListCronWorkflows reads the selected project's current workflow definitions.
func (c *Client) ListCronWorkflows(ctx context.Context, projectID int64) ([]generated.CronWorkflow, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := client.ListCronWorkflowsWithResponse(ctx, &generated.ListCronWorkflowsRequestOptions{
		PathParams: &generated.ListCronWorkflowsPath{ProjectID: projectID},
	})
	if wire == nil {
		return nil, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, err
	}
	return wire.JSON200.Workflows, nil
}

// ListCronRuns reads the newest 100 observations.
func (c *Client) ListCronRuns(ctx context.Context, projectID int64) ([]generated.CronRun, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	query := &generated.ListCronRunsQuery{Limit: new(int64(100))}
	wire, callErr := client.ListCronRunsWithResponse(ctx, &generated.ListCronRunsRequestOptions{
		PathParams: &generated.ListCronRunsPath{ProjectID: projectID},
		Query:      query,
	})
	if wire == nil {
		return nil, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, err
	}
	return wire.JSON200.Runs, nil
}

// cronParent is what a native job's parent issue tells the Jobs tab: whether
// the daemon still delivers (only an open parent does) and each job's progress.
type cronParent struct {
	open     bool
	progress map[string]cron.RecurringProgress
}

// readCronParent reads a parent issue's status and the delivery checkpoints its
// native recurring jobs keep, keyed by job UID, in one show request.
func (c *Client) readCronParent(ctx context.Context, projectID int64, parentUID string) (cronParent, error) {
	client, err := c.generatedClient()
	if err != nil {
		return cronParent{}, err
	}
	wire, callErr := client.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: projectID, Ref: parentUID},
	})
	if wire == nil {
		return cronParent{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return cronParent{}, err
	}
	issue := wire.JSON200.Issue
	return cronParent{open: issue.Status == "open", progress: cronProgressFromMetadata(issue.Metadata)}, nil
}

// cronProgressFromMetadata decodes each cron.progress.<job-uid> value. An entry
// that does not decode is skipped, so only its own job loses the preview.
func cronProgressFromMetadata(metadata map[string]any) map[string]cron.RecurringProgress {
	out := map[string]cron.RecurringProgress{}
	for key, value := range metadata {
		jobUID, ok := strings.CutPrefix(key, "cron.progress.")
		if !ok {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		var progress cron.RecurringProgress
		if json.Unmarshal(raw, &progress) == nil {
			out[jobUID] = progress
		}
	}
	return out
}
