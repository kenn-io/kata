package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kata/pkg/client/generated"
)

// RecurrenceInput is one Repeat form save. Recurrence is the recurrence the
// form read (nil when the issue had none) and IssueRevision the issue revision
// it read: a start is guarded on the issue's revision, a change or a stop on
// the recurrence's. Zone is the zone as typed; blank takes the recurrence's
// zone, then the issue's, then the caller's own, as kata schedule does.
type RecurrenceInput struct {
	recurrenceclient.Terms
	Stop          bool
	IssueRevision int64
	Recurrence    *recurrenceclient.View
}

type recurrenceAPI interface {
	SaveRecurrence(ctx context.Context, projectID int64, ref string, in RecurrenceInput) (*MutationResp, error)
}

type issueReadAPI interface {
	ReadIssue(ctx context.Context, projectID int64, ref string) (*IssueDetail, error)
}

type recurrenceReadAPI interface {
	ReadRecurrence(ctx context.Context, projectID int64, id *int64) (*recurrenceclient.View, error)
}

// recurrenceSnapshot is an issue and its recurrence as the daemon holds them.
type recurrenceSnapshot struct {
	issue Issue
	rec   *recurrenceclient.View
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
	if recurrenceclient.ParentsCronJob(issue.Metadata) {
		return nil, &mutationPreflightError{errCronParent()}
	}
	if in.Stop {
		if in.Recurrence == nil {
			return nil, &mutationPreflightError{errors.New("issue has no active recurrence; nothing was written")}
		}
		err = recurrenceclient.Stop(ctx, apiClient, projectID, *in.Recurrence, in.Actor)
	} else {
		issueZone, _ := issue.Metadata["timezone"].(string)
		zone, zoneErr := recurrenceclient.ResolveZone(in.Zone, in.Recurrence.Zone(), issueZone, localZone)
		switch {
		case errors.Is(zoneErr, recurrenceclient.ErrLocalZone):
			return nil, &mutationPreflightError{errors.New(`"Local" is not an IANA zone; enter a name like Europe/Paris`)}
		case zoneErr != nil:
			return nil, &mutationPreflightError{errors.New("no timezone known for this recurrence; enter an IANA zone like Europe/Paris")}
		}
		terms := in.Terms
		terms.Zone = zone
		if in.Recurrence != nil {
			err = recurrenceclient.Change(ctx, apiClient, projectID, *in.Recurrence, terms)
		} else {
			err = startRecurrence(ctx, apiClient, projectID, ref, wire, in, terms)
		}
	}
	if err != nil {
		if status, ok := errors.AsType[*recurrenceclient.StatusError](err); ok {
			return nil, decodeError(status.Body, status.Status, status.Method, status.URI)
		}
		return nil, err
	}
	resp := &MutationResp{Changed: true}
	if after, err := c.ReadIssue(ctx, projectID, ref); err == nil {
		resp.Issue = after.Issue
	}
	return resp, nil
}

// errCronParent is the refusal for an issue that parents a native cron job.
func errCronParent() error {
	return fmt.Errorf("%s (%s)", recurrenceclient.CronParentRefusal, recurrenceclient.CronParentHint)
}

// startRecurrence builds the template from the issue as read, minus owner and
// metadata, and creates the recurrence guarded on the revision the form read.
func startRecurrence(ctx context.Context, apiClient *generated.Client, projectID int64, ref string, shown *generated.ShowIssueResp, in RecurrenceInput, terms recurrenceclient.Terms) error {
	issue := shown.JSON200.Issue
	if issue.Revision != in.IssueRevision {
		return &APIError{
			Method: http.MethodPost, Path: fmt.Sprintf("/api/v1/projects/%d/recurrences", projectID),
			Status: http.StatusPreconditionFailed, Code: "revision_conflict",
			Message: "the issue changed since the form read it",
		}
	}
	tpl, err := recurrenceclient.TemplateFromShow(shown.Body)
	if err != nil {
		return err
	}
	tag := recurrenceclient.ETag(in.IssueRevision)
	return recurrenceclient.Start(ctx, apiClient, projectID, ref, tpl, terms, &tag)
}

// localZone names this host's zone for a recurrence that has none yet.
func localZone() string { return recurrenceclient.LocalZone(recurrenceclient.DefaultLocaltimePath) }
