package federation

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

type retainedPush struct {
	store         db.Storage
	binding       *db.FederationBinding
	validateLease func(context.Context) error
}

func (r *retainedPush) nextBatch(ctx context.Context, pending *[]db.Event) ([]db.Event, string, int64, error) {
	events := *pending
	if retainedFederationEvent(events[0], *r.binding, r.store.InstanceUID()) {
		end := 1
		for end < len(events) && retainedFederationEvent(events[end], *r.binding, r.store.InstanceUID()) {
			end++
		}
		return nil, "", 0, r.advance(ctx, pending, end)
	}
	// Isolate each event so a hub rejection identifies the event to keep local.
	if r.binding.RetainedThroughEventID > 0 {
		return nextFederationPushIngestBatch(events[:1])
	}
	return nextFederationPushIngestBatch(events)
}

func (r *retainedPush) advance(ctx context.Context, pending *[]db.Event, n int) error {
	if err := validateFederationRunnerLease(ctx, r.validateLease); err != nil {
		return err
	}
	cursor := (*pending)[n-1].ID
	if err := r.store.AdvanceFederationPushCursor(ctx, r.binding.ProjectID, cursor); err != nil {
		return err
	}
	r.binding.PushCursorEventID = cursor
	*pending = (*pending)[n:]
	return nil
}

func (r *retainedPush) resolve(ctx context.Context, pending *[]db.Event, batch []db.Event, err error) (bool, error) {
	issueUID := retainedUnknownIssue(err)
	if r.binding.RetainedThroughEventID == 0 || issueUID == "" || batch[0].IssueUID == nil || *batch[0].IssueUID != issueUID {
		return false, nil
	}
	return true, r.advance(ctx, pending, 1)
}
