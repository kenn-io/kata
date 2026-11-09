package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronAPI interface {
	ListCronJobs(context.Context, int64) (CronJobsRead, error)
	ShowCronJob(ctx context.Context, projectID int64, uid string) (CronJobRead, error)
	PutCronJob(ctx context.Context, projectID int64, uid string, write kataapi.PutCronJobBody) error
	CronJobEnabled(ctx context.Context, projectID int64, uid, expected, actor string, enabled, discard bool) (db.CronJob, error)
	ListCronWorkflows(context.Context, int64) ([]generated.CronWorkflow, error)
	ListCronRuns(context.Context, int64) ([]generated.CronRun, error)
	ShowCronRun(context.Context, int64, string) (db.CronRun, error)
	ListCronRequests(context.Context, int64) ([]db.CronRequest, error)
	RequestCron(ctx context.Context, projectID int64, uid, kind, target, actor string) error
	readCronProjectState(context.Context, int64) (cronProjectState, error)
	readCronParent(ctx context.Context, projectID int64, parentUID string) (cronParent, error)
}

// CronJobsRead is a project's job list with the missed-run cutoffs that apply.
type CronJobsRead struct {
	Jobs      []generated.CronJob              `json:"jobs"`
	Passovers map[string]*db.CronPassoverState `json:"passovers"`
}

// ListCronJobs reads the selected project's current job definitions and the
// missed-run cutoffs that apply to them.
func (c *Client) ListCronJobs(ctx context.Context, projectID int64) (CronJobsRead, error) {
	client, err := c.generatedClient()
	if err != nil {
		return CronJobsRead{}, err
	}
	wire, callErr := client.ListCronJobsWithResponse(ctx, &generated.ListCronJobsRequestOptions{
		PathParams: &generated.ListCronJobsPath{ProjectID: projectID},
	})
	if wire == nil {
		return CronJobsRead{}, callErr
	}
	var out CronJobsRead
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &out); err != nil {
		return CronJobsRead{}, err
	}
	return out, nil
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

// CronJobRead is one job with the missed-run cutoff that applies to it.
type CronJobRead struct {
	Job      db.CronJob
	Passover *db.CronPassoverState
}

// decodeCronJobRecord reads the envelope without passing options through a
// generated map, keeping the passover state beside the strict inner job.
func decodeCronJobRecord(body []byte) (CronJobRead, error) {
	var wire struct {
		Job      jsontext.Value        `json:"job"`
		Passover *db.CronPassoverState `json:"passover"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return CronJobRead{}, err
	}
	job, err := decodeCronJob(wire.Job)
	return CronJobRead{job, wire.Passover}, err
}

func decodeCronJob(body []byte) (db.CronJob, error) {
	var job kataapi.CronJob
	if err := json.Unmarshal(body, &job, json.RejectUnknownMembers(true)); err != nil {
		return db.CronJob{}, fmt.Errorf("use a newer kata to edit this job: %w", err)
	}
	return db.CronJob{CronDefinition: job.CronDefinition, Definition: job.Definition.Native()}, nil
}

// ShowCronJob reads one job, archived or not, with its missed-run cutoff.
func (c *Client) ShowCronJob(ctx context.Context, projectID int64, uid string) (CronJobRead, error) {
	client, err := c.generatedClient()
	if err != nil {
		return CronJobRead{}, err
	}
	wire, callErr := client.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{
		PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: uid},
	})
	if wire == nil {
		return CronJobRead{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return CronJobRead{}, err
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

// CronJobEnabled pauses or resumes a job under the revision counted or selected
// by the UI. discard resumes and discards the job's scheduled backlog.
func (c *Client) CronJobEnabled(ctx context.Context, projectID int64, uid, expected, actor string, enabled, discard bool) (db.CronJob, error) {
	client, err := generated.NewDefaultClient(c.base, runtime.WithHTTPClient(tuiRequestDoer{client: c, timeout: cronEnabledTimeout}))
	if err != nil {
		return db.CronJob{}, err
	}
	var resp *http.Response
	var body []byte
	if enabled {
		resume := generated.CronJobResumeBody{Actor: &actor, ExpectedEventUID: &expected}
		if discard {
			resume.DiscardMissed = &discard
		}
		wire, callErr := client.ResumeCronJobWithResponse(ctx, &generated.ResumeCronJobRequestOptions{PathParams: &generated.ResumeCronJobPath{ProjectID: projectID, CronUID: uid}, Body: &resume})
		if wire == nil {
			return db.CronJob{}, callErr
		}
		resp, body, err = wire.HTTPResponse, wire.Body, callErr
	} else {
		pause := generated.CronJobEnabledActionBody{Actor: &actor, ExpectedEventUID: &expected}
		wire, callErr := client.PauseCronJobWithResponse(ctx, &generated.PauseCronJobRequestOptions{PathParams: &generated.PauseCronJobPath{ProjectID: projectID, CronUID: uid}, Body: &pause})
		if wire == nil {
			return db.CronJob{}, callErr
		}
		resp, body, err = wire.HTTPResponse, wire.Body, callErr
	}
	if err := decodeGeneratedResponse(resp, body, err, nil); err != nil {
		return db.CronJob{}, err
	}
	read, err := decodeCronJobRecord(body)
	return read.Job, err
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
func (c *Client) RequestCron(ctx context.Context, pid int64, id, kind, target, actor string) error {
	client, err := c.generatedClient()
	if err != nil {
		return err
	}
	body := generated.CronRequestBody{UID: id, Actor: &actor}
	if kind == db.CronRequestKindRun {
		wire, callErr := client.RequestCronJobRunWithResponse(ctx, &generated.RequestCronJobRunRequestOptions{PathParams: &generated.RequestCronJobRunPath{ProjectID: pid, CronUID: target}, Body: &body})
		if wire == nil {
			return callErr
		}
		return decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil)
	}
	wire, callErr := client.RequestCronRunCancelWithResponse(ctx, &generated.RequestCronRunCancelRequestOptions{PathParams: &generated.RequestCronRunCancelPath{ProjectID: pid, RunUID: target}, Body: &body})
	if wire == nil {
		return callErr
	}
	return decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil)
}
