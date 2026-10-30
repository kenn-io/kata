package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronProgressPrefix = "cron.progress."

// reopenBacklogAPI reports how many recurring children reopening an issue
// would deliver. Clients that cannot answer simply reopen without a prompt.
type reopenBacklogAPI interface {
	ReopenBacklog(ctx context.Context, projectID int64, ref string) (int, error)
}

// ReopenBacklog sums, over every native recurring job whose progress is stored
// on the issue, the children the daemon would create once the issue is open
// again. Jobs that cannot be read or planned count as zero: the figure is a
// courtesy and must never block a reopen.
func (c *Client) ReopenBacklog(ctx context.Context, projectID int64, ref string) (int, error) {
	apiClient, err := c.generatedClient()
	if err != nil {
		return 0, err
	}
	wire, callErr := apiClient.GetIssueMetadataWithResponse(ctx, &generated.GetIssueMetadataRequestOptions{
		PathParams: &generated.GetIssueMetadataPath{ProjectID: projectID, Ref: ref},
	})
	if wire == nil {
		return 0, callErr
	}
	var body struct {
		Issue struct {
			Metadata map[string]jsontext.Value `json:"metadata"`
		} `json:"issue"`
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &body); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	total := 0
	for key, raw := range body.Issue.Metadata {
		if !strings.HasPrefix(key, cronProgressPrefix) {
			continue
		}
		var progress cron.RecurringProgress
		if json.Unmarshal(raw, &progress) != nil {
			continue
		}
		job, err := recurringJob(ctx, apiClient, projectID, strings.TrimPrefix(key, cronProgressPrefix))
		if err != nil {
			continue
		}
		count, err := cron.RecurringBacklog(job.Definition.Native(), job.DefinitionEventUID, progress, now)
		if err != nil {
			continue
		}
		total += count
	}
	return total, nil
}

func recurringJob(ctx context.Context, apiClient *generated.Client, projectID int64, uid string) (api.CronJob, error) {
	wire, callErr := apiClient.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{
		PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: uid},
	})
	if wire == nil {
		return api.CronJob{}, callErr
	}
	var body struct {
		Job api.CronJob `json:"job"`
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &body); err != nil {
		return api.CronJob{}, err
	}
	if body.Job.DeletedAt != nil {
		return api.CronJob{}, errors.New("cron job is deleted")
	}
	return body.Job, nil
}
