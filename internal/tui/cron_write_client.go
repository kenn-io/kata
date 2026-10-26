package tui

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"

	native "go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/tui/cron/records"
	"go.kenn.io/kata/pkg/client/generated"
)

// HTTPStatus exposes the native status to the copied retry logic.
func (e *APIError) HTTPStatus() int { return e.Status }

// HTTPErrorCode exposes the structured daemon error to the cron child.
func (e *APIError) HTTPErrorCode() string { return e.Code }

func decodeCronWrite(resp *http.Response, body []byte, callErr error, out any, pending records.Job, operation string) error {
	err := decodeGeneratedResponse(resp, body, callErr, out)
	if resp.StatusCode == http.StatusConflict {
		var env struct {
			Error struct {
				Data struct {
					Current records.Job `json:"current"`
				} `json:"data"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &env)
		current := env.Error.Data.Current
		current.ProjectUID = pending.ProjectUID
		current.Project = pending.Project
		return &records.ConflictError{Pending: pending, Newer: current, Operation: operation, Cause: err}
	}
	return err
}

// SaveCronJob creates or replaces a retained UID with the original definition guard.
func (c *Client) SaveCronJob(ctx context.Context, projectID int64, actor string, job records.Job) (records.Job, error) {
	api, err := c.generatedClient()
	if err != nil {
		return records.Job{}, err
	}
	var def generated.CronJobDefinition
	decoder := jsontext.NewDecoder(bytes.NewReader(job.Definition))
	if err := json.UnmarshalDecode(decoder, &def, jsonutil.PreserveNumberLiterals()); err != nil {
		return records.Job{}, err
	}
	wire, callErr := api.ReplaceCronJobWithResponse(ctx, &generated.ReplaceCronJobRequestOptions{PathParams: &generated.ReplaceCronJobPath{ProjectID: projectID, CronUID: job.UID}, Body: &generated.ReplaceCronJobBody{Actor: &actor, Name: job.Name, Definition: def, ExpectedEventUID: optionalCronGuard(job.DefinitionEventUID)}})
	if wire == nil {
		return records.Job{}, callErr
	}
	var out struct {
		Job records.Job `json:"job"`
	}
	err = decodeCronWrite(wire.HTTPResponse, wire.Body, callErr, &out, job, "save")
	out.Job.ProjectID = projectID
	out.Job.ProjectUID = fmt.Sprint(projectID)
	out.Job.Project = job.Project
	// Definition mutation receipts do not replace recorded run evidence.
	out.Job.LastRun, out.Job.LastRunKnown = job.LastRun, job.LastRunKnown
	return out.Job, err
}
func optionalCronGuard(guard string) *string {
	if guard == "" {
		return nil
	}
	return &guard
}

// ShowCronJob reads one native job for guarded write readback.
func (c *Client) ShowCronJob(ctx context.Context, projectID int64, uid string) (records.Job, error) {
	api, err := c.generatedClient()
	if err != nil {
		return records.Job{}, err
	}
	wire, callErr := api.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: uid}})
	if wire == nil {
		return records.Job{}, callErr
	}
	var out struct {
		Job records.Job `json:"job"`
	}
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	out.Job.ProjectID = projectID
	out.Job.ProjectUID = fmt.Sprint(projectID)
	return out.Job, err
}

// SaveCronWorkflow replaces a workflow with its original event guard.
func (c *Client) SaveCronWorkflow(ctx context.Context, projectID int64, actor string, workflow records.Workflow) (records.Workflow, error) {
	if _, err := native.ParseWorkflow(workflow.Definition); err != nil {
		return records.Workflow{}, err
	}
	api, err := c.generatedClient()
	if err != nil {
		return records.Workflow{}, err
	}
	var def generated.CronWorkflowDefinition
	d := jsontext.NewDecoder(bytes.NewReader(workflow.Definition))
	if err := json.UnmarshalDecode(d, &def, jsonutil.PreserveNumberLiterals()); err != nil {
		return records.Workflow{}, err
	}
	wire, callErr := api.ReplaceCronWorkflowWithResponse(ctx, &generated.ReplaceCronWorkflowRequestOptions{PathParams: &generated.ReplaceCronWorkflowPath{ProjectID: projectID, CronUID: workflow.UID}, Body: &generated.ReplaceCronWorkflowBody{Actor: &actor, Name: workflow.Name, Definition: def, ExpectedEventUID: optionalCronGuard(workflow.DefinitionEventUID)}})
	if wire == nil {
		return records.Workflow{}, callErr
	}
	var out struct {
		Workflow records.Workflow `json:"workflow"`
	}
	err = decodeCronWrite(wire.HTTPResponse, wire.Body, callErr, &out, workflow, "save workflow")
	out.Workflow.ProjectID = projectID
	out.Workflow.ProjectUID = fmt.Sprint(projectID)
	out.Workflow.Project = workflow.Project
	return out.Workflow, err
}

// ShowCronWorkflow reads one native workflow for guarded write readback.
func (c *Client) ShowCronWorkflow(ctx context.Context, projectID int64, uid string) (records.Workflow, error) {
	api, err := c.generatedClient()
	if err != nil {
		return records.Workflow{}, err
	}
	wire, callErr := api.ShowCronWorkflowWithResponse(ctx, &generated.ShowCronWorkflowRequestOptions{PathParams: &generated.ShowCronWorkflowPath{ProjectID: projectID, CronUID: uid}})
	if wire == nil {
		return records.Workflow{}, callErr
	}
	var out struct {
		Workflow records.Workflow `json:"workflow"`
	}
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	out.Workflow.ProjectID = projectID
	out.Workflow.ProjectUID = fmt.Sprint(projectID)
	return out.Workflow, err
}

// ArchiveCronJob deletes a job using its exact definition event guard.
func (c *Client) ArchiveCronJob(ctx context.Context, projectID int64, actor string, job records.Job) error {
	api, err := c.generatedClient()
	if err != nil {
		return err
	}
	wire, callErr := api.ArchiveCronJobWithResponse(ctx, &generated.ArchiveCronJobRequestOptions{PathParams: &generated.ArchiveCronJobPath{ProjectID: projectID, CronUID: job.UID}, Body: &generated.ArchiveCronJobBody{Actor: &actor, ExpectedEventUID: job.DefinitionEventUID}})
	if wire == nil {
		return callErr
	}
	return decodeCronWrite(wire.HTTPResponse, wire.Body, callErr, nil, job, "delete")
}

// Each service captures one daemon, project and actor before work is queued.
type cronProjectClient struct {
	client         *Client
	projectID      int64
	project, actor string
}

func (s cronProjectClient) SaveJob(ctx context.Context, j records.Job, _ string) (records.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	return s.client.SaveCronJob(ctx, s.projectID, s.actor, j)
}
func (s cronProjectClient) ShowJob(ctx context.Context, uid string) (records.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	return s.client.ShowCronJob(ctx, s.projectID, uid)
}
func (s cronProjectClient) ShowIssue(ctx context.Context, ref string) (records.IssueView, error) {
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return records.IssueView{}, err
	}
	if parsed.Project != "" && !strings.EqualFold(parsed.Project, s.project) {
		return records.IssueView{}, fmt.Errorf("issue reference belongs to another project")
	}
	apiRef := parsed.ShortID
	if parsed.ULID != "" {
		apiRef = parsed.ULID
	}
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	detail, err := s.client.GetIssueDetail(ctx, s.projectID, apiRef)
	if err != nil {
		return records.IssueView{}, err
	}
	if detail == nil || detail.Issue == nil {
		return records.IssueView{}, fmt.Errorf("issue not found")
	}
	i := detail.Issue
	return records.IssueView{UID: i.UID, ShortID: i.ShortID, Title: i.Title, Status: i.Status, ProjectUID: fmt.Sprint(s.projectID)}, nil
}
func (s cronProjectClient) SaveWorkflow(ctx context.Context, w records.Workflow) (records.Workflow, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	return s.client.SaveCronWorkflow(ctx, s.projectID, s.actor, w)
}
func (s cronProjectClient) ShowWorkflow(ctx context.Context, uid string) (records.Workflow, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	return s.client.ShowCronWorkflow(ctx, s.projectID, uid)
}
func (s cronProjectClient) Pause(j records.Job) (records.Job, error) {
	var raw map[string]any
	d := jsontext.NewDecoder(bytes.NewReader(j.Definition))
	if err := json.UnmarshalDecode(d, &raw, jsonutil.PreserveNumberLiterals()); err != nil {
		return records.Job{}, err
	}
	if raw == nil {
		return records.Job{}, fmt.Errorf("job definition must be an object")
	}
	enabled, _ := raw["enabled"].(bool)
	raw["enabled"] = !enabled
	var err error
	j.Definition, err = json.Marshal(raw, json.Deterministic(true), jsontext.EscapeForHTML(true))
	if err != nil {
		return records.Job{}, err
	}
	return s.SaveJob(context.Background(), j, "")
}
func (s cronProjectClient) Delete(j records.Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
	defer cancel()
	return s.client.ArchiveCronJob(ctx, s.projectID, s.actor, j)
}
