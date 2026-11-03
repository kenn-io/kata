package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/kata/internal/recurrence"
	"go.kenn.io/kata/pkg/client/generated"
)

// RecurrenceInput is one Repeat form save. Recurrence is the recurrence the
// form read (nil when the issue had none) and IssueRevision the issue revision
// it read: a start is guarded on the issue's revision, a change or a stop on
// the recurrence's. Zone is the zone as typed; blank takes the recurrence's
// zone, then the issue's, then the caller's own, as kata schedule does.
type RecurrenceInput struct {
	Actor             string
	Stop              bool
	Rule, Start, Zone string
	IssueRevision     int64
	Recurrence        *recurrence.View
}

type recurrenceAPI interface {
	SaveRecurrence(ctx context.Context, projectID int64, ref string, in RecurrenceInput) (*MutationResp, error)
}

// recurrenceSnapshot is an issue and its recurrence as the daemon holds them.
type recurrenceSnapshot struct {
	issue Issue
	rec   *recurrence.View
}

// SaveRecurrence starts, changes or stops the recurrence of an issue through
// the same shared logic as kata schedule --repeat. The daemon validates the
// rule; its refusal comes back as the error.
func (c *Client) SaveRecurrence(ctx context.Context, projectID int64, ref string, in RecurrenceInput) (*MutationResp, error) {
	apiClient, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := apiClient.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: projectID, Ref: ref},
	})
	if wire == nil {
		return nil, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, err
	}
	issue := wire.JSON200.Issue
	if recurrence.ParentsCronJob(issue.Metadata) {
		return nil, &mutationPreflightError{fmt.Errorf("%s (%s)", recurrence.CronParentRefusal, recurrence.CronParentHint)}
	}
	if in.Stop {
		if in.Recurrence == nil {
			return nil, &mutationPreflightError{errors.New("issue has no active recurrence; nothing was written")}
		}
		err = recurrence.Stop(ctx, apiClient, projectID, *in.Recurrence, in.Actor)
	} else {
		issueZone, _ := issue.Metadata["timezone"].(string)
		zone, zoneErr := recurrence.ResolveZone(in.Zone, in.Recurrence.Zone(), issueZone, recurrence.LocalZone)
		switch {
		case errors.Is(zoneErr, recurrence.ErrLocalZone):
			return nil, &mutationPreflightError{errors.New(`"Local" is not an IANA zone; enter a name like Europe/Paris`)}
		case zoneErr != nil:
			return nil, &mutationPreflightError{errors.New("no timezone known for this recurrence; enter an IANA zone like Europe/Paris")}
		}
		terms := recurrence.Terms{Rule: in.Rule, Start: in.Start, Zone: zone, Actor: in.Actor}
		if in.Recurrence != nil {
			err = recurrence.Change(ctx, apiClient, projectID, *in.Recurrence, terms)
		} else {
			err = startRecurrence(ctx, apiClient, projectID, ref, wire.JSON200, in, terms)
		}
	}
	if err != nil {
		if status, ok := errors.AsType[*recurrence.StatusError](err); ok {
			return nil, decodeError(status.Body, status.Status, status.Method, status.URI)
		}
		return nil, err
	}
	resp := &MutationResp{Changed: true}
	if after, err := c.GetIssueDetail(ctx, projectID, ref); err == nil {
		resp.Issue = after.Issue
	}
	return resp, nil
}

// startRecurrence builds the template from the issue as read, minus owner and
// metadata, and creates the recurrence guarded on the revision the form read.
func startRecurrence(ctx context.Context, apiClient *generated.Client, projectID int64, ref string, shown *generated.ShowIssueResponse, in RecurrenceInput, terms recurrence.Terms) error {
	issue := shown.Issue
	if issue.Revision != in.IssueRevision {
		return &APIError{
			Method: http.MethodPost, Path: fmt.Sprintf("/api/v1/projects/%d/recurrences", projectID),
			Status: http.StatusPreconditionFailed, Code: "revision_conflict",
			Message: "the issue changed since the form read it",
		}
	}
	labels := make([]string, 0, len(shown.Labels))
	for _, label := range shown.Labels {
		labels = append(labels, label.Label)
	}
	tag := recurrence.ETag(in.IssueRevision)
	tpl := recurrence.Template{Title: issue.Title, Body: issue.Body, Priority: issue.Priority, Labels: labels}
	return recurrence.Start(ctx, apiClient, projectID, ref, tpl, terms, &tag)
}
