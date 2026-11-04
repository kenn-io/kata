package tui

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
)

// Wait evidence is refreshed independently of retained run display records.
type cronRunWait struct {
	open     bool
	metadata map[string]any
}

type cronWaitAPI interface {
	readCronRunWait(ctx context.Context, projectID int64, issueUID string) (cronRunWait, error)
}

// The production API wrapper embeds KataAPI, which cannot promote optional
// methods. Forward wait reads explicitly so real sessions retain the capability.
func (c *undoClient) readCronRunWait(ctx context.Context, projectID int64, issueUID string) (cronRunWait, error) {
	reader, ok := c.KataAPI.(cronWaitAPI)
	if !ok {
		return cronRunWait{}, errors.New("cron attention client unavailable")
	}
	return reader.readCronRunWait(ctx, projectID, issueUID)
}

func (c *Client) readCronRunWait(ctx context.Context, projectID int64, issueUID string) (cronRunWait, error) {
	client, err := c.generatedClient()
	if err != nil {
		return cronRunWait{}, err
	}
	wire, callErr := client.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: projectID, Ref: issueUID},
		Query:      &generated.ShowIssueQuery{IncludeDeleted: new(true)},
	})
	if wire == nil {
		return cronRunWait{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return cronRunWait{}, err
	}
	issue := wire.JSON200.Issue
	return cronRunWait{open: issue.Status == "open" && issue.DeletedAt == nil, metadata: issue.Metadata}, nil
}

func cronRunWaitForActor(wait cronRunWait, actor string) cronHealthState {
	out := cronHealthState{"running", ""}
	if !wait.open {
		return out
	}
	if attention, _ := wait.metadata["work.attention"].(string); attention == "needs-human" || attention == "stuck" {
		out = cronHealthState{"blocked", "Needs human attention"}
		if attention == "stuck" {
			out.reason = "Stuck"
		}
	}
	recipient, err := notification.NormalizeRecipient(actor)
	if err != nil {
		return out
	}
	raw, err := json.Marshal(wait.metadata[notification.MetadataKey(recipient)])
	if err != nil {
		return out
	}
	request, _, err := notification.DecodeValue(raw)
	if err == nil {
		reason := "waiting on " + recipient + ": " + request.Message
		if out.state == "blocked" {
			reason += "; " + out.reason
		}
		out = cronHealthState{"blocked", truncate(sanitizeForLine(reason), 240)}
	}
	return out
}

func cronRunHealth(p cronData, r generated.CronRun) cronHealthState {
	out := cronHealthState{r.Status, ""}
	if r.Status != "running" {
		return out
	}
	if j := p.job(deref(r.JobUID)); j != nil && cronNativeParent(*j) != "" {
		return out
	}
	uid := deref(r.IssueUID)
	wait, ok := p.waits[uid]
	if !ok || p.waitErr[uid] != nil {
		return cronHealthState{"running", "attention unavailable"}
	}
	return cronRunWaitForActor(wait, r.Actor)
}

func cronRunStatus(p cronData, r generated.CronRun) string {
	if cronRunHealth(p, r).state == "blocked" {
		return "running · blocked"
	}
	return r.Status
}

// Include retained detail and loaded pages, taking the highest observation
// revision before testing status so an ended run cannot retain a stale wait.
func (m Model) cronWaitLoadedRuns() []generated.CronRun {
	if m.cron.data == nil {
		return nil
	}
	var out []generated.CronRun
	for _, r := range m.cron.data.lookup {
		out = append(out, r)
	}
	out = cronMergeRuns(out, m.cron.runPage.runs)
	out = cronMergeRuns(out, m.cron.jobPage.runs)
	out = cronMergeRuns(out, m.cron.data.runs)
	for _, r := range m.cron.data.lastRuns {
		out = cronMergeRuns(out, []generated.CronRun{r})
	}
	return out
}
