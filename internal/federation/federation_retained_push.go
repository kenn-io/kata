package federation

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// retainedPush carries the state that pushing a rejoined project's retained
// history needs across the batches of one push run.
type retainedPush struct {
	store         db.Storage
	binding       *db.FederationBinding
	hubProjectID  int64
	client        *Client
	onPulled      func(int64, []db.Event)
	validateLease func(context.Context) error
	credentials   config.FederationCredentialStore
	opts          clientpkg.Opts
	sent          map[string]bool
	// creation and recovery describe the batch nextBatch returned last.
	creation, recovery bool
}

// withRecoveryEventPublisher reports events that retained-history recovery
// commits to the daemon's open clients.
func withRecoveryEventPublisher(ctx context.Context, onPulled func(int64, []db.Event)) context.Context {
	return db.WithRecoveryEventPublisher(ctx, func(event db.Event) {
		if onPulled != nil {
			onPulled(event.ProjectID, []db.Event{event})
		}
	})
}

// nextBatch returns the next batch to push. It returns a nil batch once it has
// consumed pending events itself: it skipped events the hub already holds from
// a former actor, or it retired the rest of the pending run.
func (r *retainedPush) nextBatch(ctx context.Context, pending *[]db.Event) ([]db.Event, string, int64, error) {
	events := *pending
	instanceUID := r.store.InstanceUID()
	retained := retainedFederationEvent(events[0], *r.binding, instanceUID)
	end := 1
	for end < len(events) && retainedFederationEvent(events[end], *r.binding, instanceUID) == retained {
		end++
	}
	if retained {
		return nil, "", 0, r.advance(ctx, pending, end)
	}
	end, err := r.plan(ctx, events[:end])
	if err != nil {
		return nil, "", 0, err
	}
	batch, baseline, baselineEnd := events[:1], "", int64(0)
	if !r.recovery {
		batch, baseline, baselineEnd, err = nextFederationPushIngestBatch(events[:end])
		if err != nil {
			return nil, "", 0, err
		}
	}
	if len(batch) == 0 {
		return nil, "", 0, errors.New("empty federation push batch")
	}
	retired, err := publishRetainedComments(ctx, r.store, r.binding, r.hubProjectID, r.client, batch, r.sent, r.onPulled, r.validateLease, r.credentials, r.opts)
	if err == nil && !retired {
		retired, err = publishRetainedLinkPeers(ctx, r.store, r.binding, r.hubProjectID, r.client, batch, r.sent, r.onPulled, r.validateLease, r.credentials, r.opts)
	}
	if err != nil || retired {
		*pending = []db.Event{}
		return nil, "", 0, err
	}
	return batch, baseline, baselineEnd, nil
}

// advance moves the push cursor past the first n pending events.
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

// plan limits a run of local events to what one request can carry. A creation
// the former actor already published, or a snapshot that stands in for retained
// history, goes alone so the hub's answer to it is unambiguous.
func (r *retainedPush) plan(ctx context.Context, events []db.Event) (int, error) {
	r.creation, r.recovery = false, false
	through := r.binding.RetainedThroughEventID
	if through == 0 {
		return len(events), nil
	}
	for i, event := range events {
		if r.ownIssueEvent(event, "issue.created") && event.ID <= through {
			return alone(i, &r.creation), nil
		}
		if !r.ownIssueEvent(event, "issue.snapshot") || event.ID <= through {
			continue
		}
		snapshot, _, err := r.store.RetainedIssueSnapshot(ctx, r.binding.ProjectID, *event.IssueUID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return 0, err
		}
		if err == nil && snapshot.UID == event.UID {
			return alone(i, &r.recovery), nil
		}
	}
	return len(events), nil
}

// alone ends a batch before event i, or at it when it comes first.
func alone(i int, first *bool) int {
	if i > 0 {
		return i
	}
	*first = true
	return 1
}

func (r *retainedPush) ownIssueEvent(event db.Event, eventType string) bool {
	return event.Type == eventType && event.IssueUID != nil && event.OriginInstanceUID == r.store.InstanceUID() && event.Actor == r.binding.Actor
}

// resolve repairs a hub rejection that retained history explains. It reports
// whether pending changed and the caller should start over.
func (r *retainedPush) resolve(ctx context.Context, pending *[]db.Event, batch []db.Event, err error) (bool, error) {
	first := batch[0]
	switch {
	case r.creation && retainedInitializerAlreadyKnown(err, "issue.created", *first.IssueUID):
		return true, r.advance(ctx, pending, 1)
	case r.recovery && first.IssueUID != nil && retainedInitializerAlreadyKnown(err, "issue.snapshot", *first.IssueUID):
		*pending = []db.Event{}
		return true, r.acknowledge(ctx, first)
	}
	issueUID := retainedUnknownIssue(err)
	if issueUID == "" || r.sent[issueUID] {
		return false, nil
	}
	snapshot, sourceID, err := r.store.RetainedIssueSnapshot(ctx, r.binding.ProjectID, issueUID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	retired, err := publishRetainedLinkPeers(ctx, r.store, r.binding, r.hubProjectID, r.client, []db.Event{snapshot}, r.sent, r.onPulled, r.validateLease, r.credentials, r.opts)
	if err != nil || retired {
		*pending = []db.Event{}
		return retired, err
	}
	envelope := federationIngestEnvelope(snapshot)
	envelope.EventID = sourceID
	tokens, err := federationMoveSourceTokens(ctx, r.store, r.credentials, *r.binding, r.client.baseURL, []db.Event{snapshot}, r.opts)
	if err != nil {
		return false, err
	}
	if _, err = r.client.IngestProjectEventsWithOptions(ctx, r.hubProjectID, []api.FederationIngestEventEnvelope{envelope}, IngestProjectEventsOptions{MoveSourceTokens: tokens}); err != nil {
		return false, err
	}
	r.sent[issueUID] = true
	return true, nil
}

// acknowledge settles a snapshot the hub already holds without pushing it.
func (r *retainedPush) acknowledge(ctx context.Context, event db.Event) error {
	if err := validateFederationRunnerLease(ctx, r.validateLease); err != nil {
		return err
	}
	ackCtx, resets := db.WithFederationMoveResetCapture(ctx)
	if err := r.store.AcknowledgeFederationPush(ackCtx, r.binding.ProjectID, event.ID, []string{event.UID}); err != nil {
		return err
	}
	if r.onPulled != nil {
		for _, reset := range resets.Resets {
			r.onPulled(reset.ProjectID, []db.Event{{ID: reset.EventID, ProjectID: reset.ProjectID, Type: db.ProjectScopeResetEventType}})
		}
	}
	r.binding.PushCursorEventID = event.ID
	return nil
}
