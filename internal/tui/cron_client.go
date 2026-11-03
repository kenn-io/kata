package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronAPI interface {
	ListCronJobs(context.Context, int64) ([]generated.CronJob, error)
	ShowCronJob(ctx context.Context, projectID int64, uid string) (db.CronJob, error)
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

// decodeCronJobRecord reads the envelope without passing options through a generated map.
func decodeCronJobRecord(body []byte) (db.CronJob, error) {
	var wire struct {
		Job jsontext.Value `json:"job"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return db.CronJob{}, err
	}
	return decodeCronJob(wire.Job)
}

func decodeCronJob(body []byte) (db.CronJob, error) {
	var job kataapi.CronJob
	if err := json.Unmarshal(body, &job, json.RejectUnknownMembers(true)); err != nil {
		return db.CronJob{}, fmt.Errorf("use a newer kata to edit this job: %w", err)
	}
	return db.CronJob{CronDefinition: job.CronDefinition, Definition: job.Definition.Native()}, nil
}

// ShowCronJob reads one job, archived or not.
func (c *Client) ShowCronJob(ctx context.Context, projectID int64, uid string) (db.CronJob, error) {
	client, err := c.generatedClient()
	if err != nil {
		return db.CronJob{}, err
	}
	wire, callErr := client.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{
		PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: uid},
	})
	if wire == nil {
		return db.CronJob{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return db.CronJob{}, err
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
