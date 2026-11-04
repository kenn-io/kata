package tui

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kata/pkg/client/generated"
)

// RecurrenceInput is one Repeat form save. Recurrence is the recurrence the
// form read (nil when the issue had none) and IssueRevision the issue revision
// it read: a start is guarded on the issue's revision, a change on
// the recurrence's. Zone is the zone as typed; blank takes the recurrence's
// zone, then the issue's, then the caller's own, as kata schedule does.
type RecurrenceInput struct {
	recurrenceclient.Terms
	IssueRevision int64
	Recurrence    *recurrenceclient.View
	Stop          bool
}

type recurrenceAPI interface {
	SaveRecurrence(ctx context.Context, projectID int64, ref string, in RecurrenceInput) (*MutationResp, error)
}

// SaveRecurrence starts, changes, or stops the recurrence of an issue through
// the same shared logic as kata schedule --repeat. The daemon validates the
// rule; its refusal comes back as the error.
func (c *Client) SaveRecurrence(ctx context.Context, projectID int64, ref string, in RecurrenceInput) (*MutationResp, error) {
	apiClient, err := c.generatedClient()
	if err != nil {
		return nil, &mutationPreflightError{err}
	}
	wire, callErr := apiClient.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: projectID, Ref: ref},
	})
	if wire == nil {
		return nil, &mutationPreflightError{callErr}
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, &mutationPreflightError{err}
	}
	issue := wire.JSON200.Issue
	if recurrenceclient.ParentsCronJob(issue.Metadata) {
		return nil, &mutationPreflightError{errCronParent()}
	}
	changed := true
	if in.Stop {
		err = recurrenceclient.Stop(ctx, apiClient, issue.ProjectID, *in.Recurrence, in.Actor)
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
			changed, err = recurrenceclient.Change(ctx, apiClient, issue.ProjectID, *in.Recurrence, terms)
		} else {
			err = startRecurrence(ctx, apiClient, issue.ProjectID, issue.UID, wire, in, terms)
		}
	}

	if err != nil {
		if status, ok := errors.AsType[*recurrenceclient.StatusError](err); ok {
			return nil, decodeError(status.Body, status.Status, status.Method, status.URI)
		}
		return nil, err
	}
	return &MutationResp{Changed: changed, Issue: &Issue{UID: issue.UID, ShortID: issue.ShortID, ProjectID: issue.ProjectID}}, nil
}

// errCronParent is the refusal for an issue that parents a native cron job.
func errCronParent() error {
	return fmt.Errorf("%s (%s)", recurrenceclient.CronParentRefusal, recurrenceclient.CronParentHint)
}

// startRecurrence builds the template from the issue as read, minus owner and
// metadata, and creates the recurrence guarded on the revision the form read.
func startRecurrence(ctx context.Context, apiClient *generated.Client, projectID int64, ref string, shown *generated.ShowIssueResp, in RecurrenceInput, terms recurrenceclient.Terms) error {
	if err := requireAPIVersion(ctx, apiClient, recurrenceclient.APIVersionIfMatch, "starting a recurrence requires daemon API "+recurrenceclient.APIVersionIfMatch+" or newer"); err != nil {
		return &mutationPreflightError{err}
	}
	tpl, err := recurrenceclient.TemplateFromShow(shown.Body)
	if err != nil {
		return &mutationPreflightError{err}
	}
	tag := recurrenceclient.ETag(in.IssueRevision)
	return recurrenceclient.Start(ctx, apiClient, projectID, ref, tpl, terms, &tag)
}

// localZone names this host's zone for a recurrence that has none yet.
func localZone() string { return recurrenceclient.LocalZone(recurrenceclient.DefaultLocaltimePath) }
