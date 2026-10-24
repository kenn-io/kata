package tui

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/tui/cron/records"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronAPI uses the same transport, authentication and selected daemon as issues.
// It stays separate from KataAPI so issue-only clients do not pretend to support cron.
type CronAPI interface {
	ListCronJobs(context.Context, int64) ([]records.Job, error)
	ListCronWorkflows(context.Context, int64) ([]records.Workflow, error)
	ListCronRuns(context.Context, int64, string, string) (records.RunPage, error)
	ShowCronRun(context.Context, int64, string) (records.Run, error)
}

// ListCronJobs reads the selected project catalog through the active daemon transport.
func (c *Client) ListCronJobs(ctx context.Context, projectID int64) ([]records.Job, error) {
	api, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := api.ListCronJobsWithResponse(ctx, &generated.ListCronJobsRequestOptions{PathParams: &generated.ListCronJobsPath{ProjectID: projectID}})
	if wire == nil {
		return nil, callErr
	}
	var out struct {
		Jobs []records.Job `json:"jobs"`
	}
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	for i := range out.Jobs {
		out.Jobs[i].ProjectID = projectID
		out.Jobs[i].ProjectUID = fmt.Sprint(projectID)
	}
	return out.Jobs, err
}

// ListCronWorkflows reads the selected project workflow catalog.
func (c *Client) ListCronWorkflows(ctx context.Context, projectID int64) ([]records.Workflow, error) {
	api, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := api.ListCronWorkflowsWithResponse(ctx, &generated.ListCronWorkflowsRequestOptions{PathParams: &generated.ListCronWorkflowsPath{ProjectID: projectID}})
	if wire == nil {
		return nil, callErr
	}
	var out struct {
		Workflows []records.Workflow `json:"workflows"`
	}
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	for i := range out.Workflows {
		out.Workflows[i].ProjectID = projectID
		out.Workflows[i].ProjectUID = fmt.Sprint(projectID)
	}
	return out.Workflows, err
}

// ListCronRuns reads one bounded native history page and its cursor.
func (c *Client) ListCronRuns(ctx context.Context, projectID int64, jobUID, before string) (records.RunPage, error) {
	api, err := c.generatedClient()
	if err != nil {
		return records.RunPage{}, err
	}
	q := &generated.ListCronRunsQuery{Limit: new(int64(100))}
	if jobUID != "" {
		q.JobUID = &jobUID
	}
	if before != "" {
		q.BeforeUID = &before
	}
	wire, callErr := api.ListCronRunsWithResponse(ctx, &generated.ListCronRunsRequestOptions{PathParams: &generated.ListCronRunsPath{ProjectID: projectID}, Query: q})
	if wire == nil {
		return records.RunPage{}, callErr
	}
	var out records.RunPage
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	for i := range out.Runs {
		out.Runs[i].ProjectID = projectID
		out.Runs[i].ProjectKey = fmt.Sprint(projectID)
	}
	return out, err
}

// ShowCronRun reads the current recorded evidence for one run.
func (c *Client) ShowCronRun(ctx context.Context, projectID int64, uid string) (records.Run, error) {
	api, err := c.generatedClient()
	if err != nil {
		return records.Run{}, err
	}
	wire, callErr := api.ShowCronRunWithResponse(ctx, &generated.ShowCronRunRequestOptions{PathParams: &generated.ShowCronRunPath{ProjectID: projectID, RunUID: uid}})
	if wire == nil {
		return records.Run{}, callErr
	}
	var out struct {
		Run records.Run `json:"run"`
	}
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	out.Run.ProjectID = projectID
	out.Run.ProjectKey = fmt.Sprint(projectID)
	return out.Run, err
}

func cronAPIFor(api KataAPI) CronAPI {
	if wrapped, ok := api.(*undoClient); ok {
		api = wrapped.KataAPI
	}
	cron, _ := api.(CronAPI)
	return cron
}
