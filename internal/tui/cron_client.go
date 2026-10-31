package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronAPI interface {
	ListCronJobs(context.Context, int64) ([]generated.CronJob, error)
	ShowCronJob(ctx context.Context, projectID int64, uid string) (CronJobRecord, error)
	PutCronJob(ctx context.Context, projectID int64, uid string, write kataapi.PutCronJobBody) error
	ListCronWorkflows(context.Context, int64) ([]generated.CronWorkflow, error)
	ListCronRuns(context.Context, int64) ([]generated.CronRun, error)
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

// CronJobRecord is one job read for editing. Its definition is decoded from
// the response body itself, so adapter options keep unknown keys and exact
// numbers instead of passing through a generated map.
type CronJobRecord struct {
	UID, Name, DefinitionEventUID string
	Archived                      bool
	Definition                    cron.JobDefinition
}

func decodeCronJobRecord(body []byte) (CronJobRecord, error) {
	var wire struct {
		Job struct {
			UID                string         `json:"uid"`
			Name               string         `json:"name"`
			DefinitionEventUID string         `json:"definition_event_uid"`
			DeletedAt          *string        `json:"deleted_at"`
			Definition         jsontext.Value `json:"definition"`
		} `json:"job"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return CronJobRecord{}, err
	}
	record := CronJobRecord{UID: wire.Job.UID, Name: wire.Job.Name, DefinitionEventUID: wire.Job.DefinitionEventUID, Archived: wire.Job.DeletedAt != nil}
	// A member this kata does not know would be dropped by the next write.
	definition, err := cron.DecodeJob(wire.Job.Definition)
	if err != nil {
		return CronJobRecord{}, fmt.Errorf("the job has definition members this kata does not know, so editing it here would drop them; use a newer kata (%w)", err)
	}
	record.Definition = definition
	return record, nil
}

// ShowCronJob reads one job, archived or not.
func (c *Client) ShowCronJob(ctx context.Context, projectID int64, uid string) (CronJobRecord, error) {
	client, err := c.generatedClient()
	if err != nil {
		return CronJobRecord{}, err
	}
	wire, callErr := client.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{
		PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: uid},
	})
	if wire == nil {
		return CronJobRecord{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return CronJobRecord{}, err
	}
	return decodeCronJobRecord(wire.Body)
}

// PutCronJob creates or replaces a job, the same call `kata cron job create`
// and `update` make. The body reaches the generated type through its own
// decoder, which keeps option numbers exact.
func (c *Client) PutCronJob(ctx context.Context, projectID int64, uid string, write kataapi.PutCronJobBody) error {
	client, err := c.generatedClient()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(write)
	if err != nil {
		return err
	}
	var body generated.PutCronJobBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	wire, callErr := client.ReplaceCronJobWithResponse(ctx, &generated.ReplaceCronJobRequestOptions{
		PathParams: &generated.ReplaceCronJobPath{ProjectID: projectID, CronUID: uid},
		Body:       &body,
	})
	if wire == nil {
		return callErr
	}
	return decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil)
}
