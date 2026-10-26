package tui

import (
	"context"
	"fmt"
	"net/http"

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

// CronPeekAPI is optional so issue-only and older clients remain usable.
type CronPeekAPI interface {
	CronIssuePeek(context.Context, int64, string) (records.IssuePeek, error)
}

// CronIssuePeek reads the issue and its comments together through the existing
// native issue endpoint; GetIssueDetail intentionally omits those comments.
// An issue keeps its UID when it moves projects. The run's project answers for
// unmoved issues, including under project-scoped authority; a moved one falls
// back to the UID read, which the daemon authorizes against its new project.
func (c *Client) CronIssuePeek(ctx context.Context, projectID int64, uid string) (records.IssuePeek, error) {
	body, err := c.showIssue(ctx, projectID, uid)
	if records.Status(err) == http.StatusNotFound {
		body, err = c.showIssueByUID(ctx, uid)
	}
	if err != nil {
		return records.IssuePeek{}, err
	}
	peek := records.IssuePeek{IssueUID: body.Issue.UID, ShortID: body.Issue.ShortID, Title: body.Issue.Title, ProjectID: body.Issue.ProjectID}
	for _, comment := range body.Comments {
		if peek.Latest == nil || comment.CreatedAt.After(peek.Latest.CreatedAt) || comment.CreatedAt.Equal(peek.Latest.CreatedAt) && comment.UID > peek.Latest.UID {
			peek.Latest = &records.PeekComment{UID: comment.UID, Author: comment.Author, Body: comment.Body, CreatedAt: comment.CreatedAt}
		}
	}
	return peek, nil
}

func (c *Client) showIssueByUID(ctx context.Context, uid string) (*showIssueBody, error) {
	api, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := api.ShowIssueByUIDWithResponse(ctx, &generated.ShowIssueByUIDRequestOptions{PathParams: &generated.ShowIssueByUIDPath{UID: uid}})
	if wire == nil {
		return nil, callErr
	}
	var body showIssueBody
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &body); err != nil {
		return nil, err
	}
	return &body, nil
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
		if run := out.Jobs[i].LastRun; run != nil {
			run.ProjectID, run.ProjectKey = projectID, fmt.Sprint(projectID)
		}
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
