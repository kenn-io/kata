package federation

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/httpurl"
)

// retainedPush carries the state that pushing a rejoined project's retained
// history needs across the batches of one push run.
type retainedPush struct {
	store          db.Storage
	binding        *db.FederationBinding
	hubProjectID   int64
	client         *Client
	validateLease  func(context.Context) error
	credentials    config.FederationCredentialStore
	opts           clientpkg.Opts
	onEvents       func(int64, []db.Event)
	sent           map[string]bool
	visiting       map[string]bool
	commentHistory map[string]map[string]bool
	// creation and recovery describe the batch nextBatch returned last.
	creation, recovery bool
}

// nextBatch returns the next batch to push. It returns a nil batch after
// advancing past history from the former actor or an accepted recovery copy.
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
	for i, event := range batch {
		if err := r.prepareEventDependencies(ctx, event, batch[:i]); err != nil {
			if i == 0 {
				return nil, "", 0, err
			}
			r.creation, r.recovery = false, false
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
// relationship and checks replies whose parents may have been skipped at
// rejoin. Events in preceding belong to the same request and can satisfy a
// reply when they create its parent comment.
func (r *retainedPush) prepareEventDependencies(ctx context.Context, event db.Event, preceding []db.Event) error {
	peers, err := retainedLinkAdditions(event)
	if err != nil {
		return err
	}
	for _, peerUID := range peers {
		if err := r.publishRetainedIssue(ctx, peerUID); err != nil {
			return err
		}
	}
	return r.prepareCommentDependencies(ctx, event, preceding)
}

func retainedLinkAdditions(event db.Event) ([]string, error) {
	if event.Type == "issue.moved" {
		remote := db.FederationMoveSnapshotEvent(db.RemoteEvent{Type: event.Type, Payload: []byte(event.Payload)})
		event.Type, event.Payload = remote.Type, string(remote.Payload)
	} else if event.Type != "issue.linked" && event.Type != "issue.links_changed" &&
		event.Type != "issue.created" && event.Type != "issue.snapshot" {
		return nil, nil
	}
	fold := db.FoldEvent{UID: event.UID, Type: event.Type, IssueUID: db.EventIssueUID(event.IssueUID), Payload: []byte(event.Payload)}
	peers, err := db.DirectLinkPeers(fold)
	if err != nil {
		return nil, err
	}
	if event.Type == "issue.links_changed" {
		if removed, ok := db.StringValue(db.PayloadMap([]byte(event.Payload))["parent_removed_uid"]); ok {
			delete(peers, removed)
		}
	}
	result := make([]string, 0, len(peers))
	for uid := range peers {
		result = append(result, uid)
	}
	slices.Sort(result)
	return result, nil
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
	client, hubProjectID := r.client, r.hubProjectID
	if issue.ProjectID != binding.ProjectID {
		binding, err = r.store.FederationBindingByProject(ctx, issue.ProjectID)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	retained, err := r.hasRetainedIssue(ctx, issue, binding)
	if err != nil || !retained {
		return err
	}
	if issue.ProjectID != r.binding.ProjectID {
		client, hubProjectID, err = r.retainedEndpointClient(ctx, issueUID, binding)
		if err != nil {
			return err
		}
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
	peers, err := retainedLinkAdditions(snapshot)
	if err != nil {
		return err
	}
	for _, peerUID := range peers {
		if err := r.publishRetainedIssue(ctx, peerUID); err != nil {
			return err
		}
	}
	if err := r.prepareCommentDependencies(ctx, snapshot, nil); err != nil {
		return err
	}
	if err := validateFederationRunnerLease(ctx, r.validateLease); err != nil {
		return err
	}
	envelope := federationIngestEnvelope(snapshot)
	envelope.EventID = sourceID
	tokens, err := federationMoveSourceTokens(ctx, r.store, r.credentials, binding, client.baseURL, []db.Event{snapshot}, r.opts)
	if err != nil {
		return err
	}
	_, err = client.IngestProjectEventsWithOptions(ctx, hubProjectID, []api.FederationIngestEventEnvelope{envelope}, IngestProjectEventsOptions{MoveSourceTokens: tokens})
	if err != nil && !retainedInitializerAlreadyKnown(err, "issue.snapshot", issueUID) {
		return err
	}
	r.sent[issueUID] = true
	return nil
}

func (r *retainedPush) hasRetainedIssue(ctx context.Context, issue db.Issue, binding db.FederationBinding) (bool, error) {
	if binding.RetainedThroughEventID == 0 {
		return false, nil
	}
	events, err := r.store.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: issue.ProjectID, ThroughID: binding.RetainedThroughEventID,
		IssueUID: issue.UID, Types: []string{"issue.created", "issue.snapshot", "issue.moved"}, Limit: 1,
	})
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if retainedFederationEvent(event, binding, r.store.InstanceUID()) {
			return true, nil
		}
	}
	return false, nil
}

func (r *retainedPush) retainedEndpointClient(ctx context.Context, issueUID string, binding db.FederationBinding) (*Client, int64, error) {
	problem := func(reason string) (*Client, int64, error) {
		return nil, 0, fmt.Errorf("retained link endpoint %s: %s", issueUID, reason)
	}
	if !binding.IsDirectSpoke() || !binding.Enabled || !binding.PushEnabled {
		return problem("its project has no enabled direct push binding")
	}
	destinationURL, err := httpurl.CanonicalHTTPBaseURL(r.client.baseURL)
	if err != nil {
		return problem("the current hub origin is invalid")
	}
	endpointURL, err := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if err != nil || endpointURL != destinationURL {
		return problem("its project is bound to a different hub origin")
	}
	credential, found, err := r.credentials.FederationCredential(ctx, binding.HubProjectUID)
	if err != nil {
		return nil, 0, fmt.Errorf("retained link endpoint %s: read its hub credential: %w", issueUID, err)
	}
	if !found || credential.Token == "" || credential.LeavePending || credential.RelayEnrollmentPending {
		return problem("its project has no active hub credential")
	}
	if credential.HubProjectID != 0 && credential.HubProjectID != binding.HubProjectID ||
		credential.Actor != "" && credential.Actor != binding.Actor {
		return problem("its hub credential does not match the project binding")
	}
	credentialURL := credential.HubURL
	if credentialURL == "" {
		credentialURL = binding.HubURL
	}
	credentialOrigin, err := httpurl.CanonicalHTTPBaseURL(credentialURL)
	if err != nil || credentialOrigin != endpointURL {
		return problem("its hub credential targets a different origin")
	}
	if credential.Capabilities != "" && !federationCredentialHasCapability(credential.Capabilities, "push") {
		return problem("its hub credential has no push capability")
	}
	transportCredential := config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	client, err := NewClient(ctx, binding.HubURL, transportCredential.Token, clientOptsForCredential(r.opts, transportCredential))
	if err != nil {
		return nil, 0, fmt.Errorf("retained link endpoint %s: configure its hub credential: %w", issueUID, err)
	}
	return client, binding.HubProjectID, nil
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
	snapshot, sourceID, err := r.snapshot(ctx, issueUID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	envelope := federationIngestEnvelope(snapshot)
	envelope.EventID = sourceID
	if err := r.prepareEventDependencies(ctx, snapshot, nil); err != nil {
		return false, err
	}
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
