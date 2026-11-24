package federation

import (
	"context"
	"errors"
	"maps"
	"slices"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// retainedPush carries the state that pushing a rejoined project's retained
// history needs across the batches of one push run.
type retainedPush struct {
	store        db.Storage
	binding      *db.FederationBinding
	hubProjectID int64
	// Retry a rejected batch one event at a time to avoid resending its tail.
	retryThrough  int64
	client        *Client
	validateLease func(context.Context) error
	onEvents      func(int64, []db.Event)
	sent          map[string]bool
	visiting      map[string]bool
	// creation and recovery describe the batch nextBatch returned last.
	creation, recovery bool
}

// nextBatch returns the next batch to push. It returns a nil batch after
// advancing past history from the former actor or an accepted recovery copy.
func (r *retainedPush) nextBatch(ctx context.Context, pending *[]db.Event) ([]db.Event, string, int64, error) {
	events := *pending
	if r.retryThrough != 0 {
		if events[0].ID <= r.retryThrough {
			events = events[:1]
		} else {
			r.retryThrough = 0
		}
	}
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
	for i, event := range batch {
		if err := r.prepareEventDependencies(ctx, event); err != nil {
			if i == 0 {
				return nil, "", 0, err
			}
			prefix, prefixBaseline, prefixEnd, sizeErr := nextFederationPushIngestBatch(batch[:i])
			if sizeErr != nil {
				return nil, "", 0, sizeErr
			}
			return prefix, prefixBaseline, prefixEnd, nil
		}
	}
	return batch, baseline, baselineEnd, nil
}

// prepareEventDependencies publishes retained link endpoints before their
// relationship.
func (r *retainedPush) prepareEventDependencies(ctx context.Context, event db.Event) error {
	remote := db.FederationMoveSnapshotEvent(db.RemoteEvent{Type: event.Type, Payload: []byte(event.Payload)})
	event.Type, event.Payload = remote.Type, string(remote.Payload)
	if event.Type != "issue.unlinked" {
		linkType := event.Type
		if linkType == "issue.snapshot" {
			linkType = "issue.created"
		}
		peers, err := db.DirectLinkPeers(db.FoldEvent{UID: event.UID, Type: linkType, IssueUID: db.EventIssueUID(event.IssueUID), Payload: remote.Payload})
		if err != nil {
			return err
		}
		if event.Type == "issue.links_changed" {
			removed, _ := db.StringValue(db.PayloadMap(remote.Payload)["parent_removed_uid"])
			delete(peers, removed)
		}
		for _, peerUID := range slices.Sorted(maps.Keys(peers)) {
			if err := r.publishRetainedIssue(ctx, peerUID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *retainedPush) publishRetainedIssue(ctx context.Context, issueUID string) error {
	if r.sent[issueUID] || r.visiting[issueUID] {
		return nil
	}
	issue, err := r.store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	binding := *r.binding
	if issue.ProjectID != binding.ProjectID {
		return nil
	}
	retained, err := db.RetainedIssueEvent(ctx, r.store, binding, issue.ProjectID, issueUID,
		[]string{"issue.created", "issue.snapshot", "issue.moved"}, nil)
	if err != nil || !retained {
		return err
	}
	if r.visiting == nil {
		r.visiting = make(map[string]bool)
	}
	r.visiting[issueUID] = true
	defer delete(r.visiting, issueUID)
	snapshot, sourceID, created, err := r.store.RetainedIssueSnapshot(ctx, binding.ProjectID, issueUID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if created && r.onEvents != nil {
		r.onEvents(binding.ProjectID, []db.Event{snapshot})
	}
	if err := r.prepareEventDependencies(ctx, snapshot); err != nil {
		return err
	}
	if err := validateFederationRunnerLease(ctx, r.validateLease); err != nil {
		return err
	}
	envelope := federationIngestEnvelope(snapshot)
	envelope.EventID = sourceID
	_, err = r.client.IngestProjectEventsWithOptions(ctx, r.hubProjectID, []api.FederationIngestEventEnvelope{envelope}, IngestProjectEventsOptions{})
	if err != nil && !retainedInitializerAlreadyKnown(err, "issue.snapshot", issueUID) {
		return err
	}
	r.sent[issueUID] = true
	return nil
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
		if !r.ownIssueEvent(event, "issue.snapshot") {
			continue
		}
		snapshot, _, err := r.snapshot(ctx, *event.IssueUID)
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

func (r *retainedPush) snapshot(ctx context.Context, issueUID string) (db.Event, int64, error) {
	event, sourceID, created, err := r.store.RetainedIssueSnapshot(ctx, r.binding.ProjectID, issueUID)
	if err == nil && created && r.onEvents != nil {
		r.onEvents(r.binding.ProjectID, []db.Event{event})
	}
	return event, sourceID, err
}

// resolve repairs a hub rejection that retained history explains. It reports
// whether pending changed and the caller should start over.
func (r *retainedPush) resolve(ctx context.Context, pending *[]db.Event, batch []db.Event, err error) (bool, error) {
	first := batch[0]
	switch {
	case r.creation && retainedInitializerAlreadyKnown(err, "issue.created", *first.IssueUID):
		return true, r.advance(ctx, pending, 1)
	case r.recovery && retainedInitializerAlreadyKnown(err, "issue.snapshot", *first.IssueUID):
		r.sent[*first.IssueUID] = true
		return true, r.advance(ctx, pending, 1)
	}
	issueUID := retainedUnknownIssue(err)
	if issueUID == "" || r.sent[issueUID] {
		return false, nil
	}
	if err := r.publishRetainedIssue(ctx, issueUID); err != nil {
		return false, err
	}
	if r.sent[issueUID] && r.retryThrough == 0 {
		r.retryThrough = batch[len(batch)-1].ID
	}
	return r.sent[issueUID], nil
}
