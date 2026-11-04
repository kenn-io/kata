package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronAPI interface {
	ListCronRequests(context.Context, int64) ([]db.CronRequest, error)
	ShowCronRun(context.Context, int64, string) (db.CronRun, error)
	RequestCron(context.Context, int64, string, string, string, string) (db.RequestCronOut, error)
	readCronProjectState(context.Context, int64) (cronProjectState, error)
	CronJobEnabled(ctx context.Context, projectID int64, uid, expected, actor string, enabled bool) (db.CronJob, error)
	ListCronJobs(context.Context, int64) ([]generated.CronJob, error)
	ShowCronJob(ctx context.Context, projectID int64, uid string) (db.CronJob, error)
	PutCronJob(ctx context.Context, projectID int64, uid string, write kataapi.PutCronJobBody) error
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
		Query:      &generated.ShowIssueQuery{IncludeDeleted: new(true)},
	})
	if wire == nil {
		return cronParent{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return cronParent{}, err
	}
	issue := wire.JSON200.Issue
	return cronParent{open: issue.Status == "open" && issue.DeletedAt == nil, progress: cronProgressFromMetadata(issue.Metadata)}, nil
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
		if progress, err := cron.DecodeRecurringProgress(raw); err == nil {
			out[jobUID] = progress
		}
	}
	return out
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

// Allow the daemon's 15-second federation forward plus response overhead.
const cronEnabledTimeout = 20 * time.Second

// CronJobEnabled changes enabled under the revision counted or selected by the UI.
func (c *Client) CronJobEnabled(ctx context.Context, projectID int64, uid, expected, actor string, enabled bool) (db.CronJob, error) {
	client, err := generated.NewDefaultClient(c.base, runtime.WithHTTPClient(tuiRequestDoer{client: c, timeout: cronEnabledTimeout}))
	if err != nil {
		return db.CronJob{}, err
	}
	body := generated.CronJobEnabledActionBody{Actor: &actor, ExpectedEventUID: &expected}
	if enabled {
		wire, callErr := client.ResumeCronJobWithResponse(ctx, &generated.ResumeCronJobRequestOptions{PathParams: &generated.ResumeCronJobPath{ProjectID: projectID, CronUID: uid}, Body: &body})
		if wire == nil {
			return db.CronJob{}, callErr
		}
		if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
			return db.CronJob{}, err
		}
		return decodeCronJobRecord(wire.Body)
	}
	wire, callErr := client.PauseCronJobWithResponse(ctx, &generated.PauseCronJobRequestOptions{PathParams: &generated.PauseCronJobPath{ProjectID: projectID, CronUID: uid}, Body: &body})
	if wire == nil {
		return db.CronJob{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return db.CronJob{}, err
	}
	return decodeCronJobRecord(wire.Body)
}

// cronProjectState is optional on older daemons. Keep it separate from the
// strictly decoded inner job records.
type cronProjectState struct {
	pause    *db.CronProjectPause
	controls []string
}

func (c *Client) readCronProjectState(ctx context.Context, projectID int64) (cronProjectState, error) {
	client, err := c.generatedClient()
	if err != nil {
		return cronProjectState{}, err
	}
	wire, callErr := client.GetCronCapabilitiesWithResponse(ctx, &generated.GetCronCapabilitiesRequestOptions{PathParams: &generated.GetCronCapabilitiesPath{ProjectID: projectID}})
	if wire == nil {
		return cronProjectState{}, callErr
	}
	var response struct {
		ProjectPause *db.CronProjectPause `json:"project_pause"`
		Controls     []string             `json:"controls"`
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &response); err != nil {
		return cronProjectState{}, err
	}
	return cronProjectState{pause: response.ProjectPause, controls: response.Controls}, nil
}

// ShowCronRun reads the live observation used to confirm cancellation eligibility.
func (c *Client) ShowCronRun(ctx context.Context, pid int64, id string) (db.CronRun, error) {
	client, err := c.generatedClient()
	if err != nil {
		return db.CronRun{}, err
	}
	wire, callErr := client.ShowCronRunWithResponse(ctx, &generated.ShowCronRunRequestOptions{PathParams: &generated.ShowCronRunPath{ProjectID: pid, RunUID: id}})
	if wire == nil {
		return db.CronRun{}, callErr
	}
	var response struct {
		Run db.CronRun `json:"run"`
	}
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &response)
	return response.Run, err
}

// ListCronRequests reads retained identities and their current derived state.
func (c *Client) ListCronRequests(ctx context.Context, pid int64) ([]db.CronRequest, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := client.ListCronRequestsWithResponse(ctx, &generated.ListCronRequestsRequestOptions{PathParams: &generated.ListCronRequestsPath{ProjectID: pid}})
	if wire == nil {
		return nil, callErr
	}
	var response struct {
		Requests []db.CronRequest `json:"requests"`
	}
	err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &response)
	return response.Requests, err
}

// RequestCron submits one caller-retained identity without implicit new retries.
func (c *Client) RequestCron(ctx context.Context, pid int64, id, kind, target, actor string) (db.RequestCronOut, error) {
	client, err := c.generatedClient()
	if err != nil {
		return db.RequestCronOut{}, err
	}
	body := generated.CronRequestBody{UID: id, Actor: &actor}
	var out db.RequestCronOut
	if kind == db.CronRequestKindRun {
		wire, callErr := client.RequestCronJobRunWithResponse(ctx, &generated.RequestCronJobRunRequestOptions{PathParams: &generated.RequestCronJobRunPath{ProjectID: pid, CronUID: target}, Body: &body})
		if wire == nil {
			return out, callErr
		}
		err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	} else {
		wire, callErr := client.RequestCronRunCancelWithResponse(ctx, &generated.RequestCronRunCancelRequestOptions{PathParams: &generated.RequestCronRunCancelPath{ProjectID: pid, RunUID: target}, Body: &body})
		if wire == nil {
			return out, callErr
		}
		err = decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out)
	}
	return out, err
}
