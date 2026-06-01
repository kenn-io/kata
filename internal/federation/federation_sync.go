package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

const federationPollLimit = 1000
const defaultClientTimeout = 5 * time.Second

// ErrFederationResetRequired reports a hub that still requires reset after the
// spoke refreshed federation metadata and replayed from the new horizon.
var ErrFederationResetRequired = errors.New("federation reset required")

// ErrFederationResetBlockedByPendingPush reports that a spoke cannot safely
// reset because it still has local-origin events that the hub has not accepted.
var ErrFederationResetBlockedByPendingPush = db.ErrFederationResetBlockedByPendingPush

// ErrFederationPushQuarantined reports that an unresolved poisoned push batch
// requires explicit operator action before push can continue.
var ErrFederationPushQuarantined = db.ErrFederationPushQuarantined

// ErrFederationResetBlockedByQuarantine reports that reset is blocked by an
// unresolved poisoned federation batch.
var ErrFederationResetBlockedByQuarantine = db.ErrFederationResetBlockedByQuarantine

// SyncFederationOnce pulls one spoke binding from its configured hub.
func SyncFederationOnce(
	ctx context.Context,
	store db.Storage,
	binding db.FederationBinding,
	creds config.FederationCredential,
) error {
	return SyncFederationOnceWithPulledEvents(ctx, store, binding, creds, clientOptsWithDefault(clientpkg.Opts{}), nil)
}

// SyncFederationOnceWithPulledEvents is SyncFederationOnce with a post-commit
// callback for freshly inserted hub-origin events. The daemon uses this to
// fan pulled events out to SSE subscribers and hooks without making this
// package depend on daemon internals.
func SyncFederationOnceWithPulledEvents(
	ctx context.Context,
	store db.Storage,
	binding db.FederationBinding,
	creds config.FederationCredential,
	opts clientpkg.Opts,
	onPulledEvents func(projectID int64, events []db.Event),
) error {
	hubURL := creds.HubURL
	if hubURL == "" {
		hubURL = binding.HubURL
	}
	hubProjectID := creds.HubProjectID
	if hubProjectID == 0 {
		hubProjectID = binding.HubProjectID
	}
	if binding.PushEnabled {
		if err := store.RecordFederationSyncPushStarted(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
			return err
		}
	} else {
		if err := store.RecordFederationSyncPullStarted(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
			return err
		}
	}
	client, err := NewClient(ctx, hubURL, creds.Token, clientOptsWithDefault(opts))
	if err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	runStartBinding := binding
	if binding.PushEnabled {
		for {
			if _, err := store.ActiveFederationQuarantine(ctx, binding.ProjectID, db.FederationQuarantineDirectionPush); err == nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, ErrFederationPushQuarantined)
			} else if err != nil && !errors.Is(err, db.ErrNotFound) {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
			pending, err := store.PendingFederationPushEvents(
				ctx, binding.ProjectID, store.InstanceUID(), binding.PushCursorEventID, federationPollLimit)
			if err != nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
			if len(pending) == 0 {
				break
			}
			ack, err := client.IngestProjectEvents(ctx, hubProjectID, federationIngestEnvelopes(pending))
			if err != nil {
				if isPoisonedFederationPushError(err) {
					if qErr := recordFederationPushQuarantine(ctx, store, binding.ProjectID, pending, err); qErr != nil {
						return recordFederationSyncError(ctx, store, binding.ProjectID, errors.Join(err, qErr))
					}
				}
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
			if ack.PushCursorEventID <= binding.PushCursorEventID {
				return recordFederationSyncError(ctx, store, binding.ProjectID,
					errors.New("federation push cursor did not advance"))
			}
			lastSubmittedEventID := pending[len(pending)-1].ID
			if ack.PushCursorEventID > lastSubmittedEventID {
				return recordFederationSyncError(ctx, store, binding.ProjectID,
					errors.New("federation push cursor advanced beyond submitted batch"))
			}
			if err := federationFailpoint("before_spoke_push_cursor_advance"); err != nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
			if err := store.AdvanceFederationPushCursor(ctx, binding.ProjectID, ack.PushCursorEventID); err != nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
			binding.PushCursorEventID = ack.PushCursorEventID
		}
		if err := store.RecordFederationSyncPushSuccess(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
			return err
		}
		if err := store.RecordFederationSyncPullStarted(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
			return err
		}
	}
	body, err := client.PollProjectEvents(ctx, hubProjectID, binding.PullCursorEventID, federationPollLimit)
	if err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	if body.ResetRequired {
		if binding.PushEnabled {
			if _, err := store.ActiveFederationQuarantine(ctx, binding.ProjectID, db.FederationQuarantineDirectionPush); err == nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, ErrFederationResetBlockedByQuarantine)
			} else if err != nil && !errors.Is(err, db.ErrNotFound) {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
			pending, err := store.PendingFederationPushEvents(
				ctx, binding.ProjectID, store.InstanceUID(), binding.PushCursorEventID, 1)
			if err != nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
			if len(pending) > 0 {
				return recordFederationSyncError(ctx, store, binding.ProjectID, ErrFederationResetBlockedByPendingPush)
			}
		}
		meta, err := client.ProjectFederation(ctx, hubProjectID)
		if err != nil {
			return recordFederationSyncError(ctx, store, binding.ProjectID, err)
		}
		cursor := meta.ReplayHorizonEventID - 1
		if cursor < 0 {
			cursor = 0
		}
		if binding.PushEnabled {
			if err := store.ResetFederatedProjectIfNoPendingPush(
				ctx, binding.ProjectID, meta.ReplayHorizonEventID, cursor, store.InstanceUID(), binding.PushCursorEventID); err != nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
		} else {
			if err := store.ResetFederatedProject(ctx, binding.ProjectID, meta.ReplayHorizonEventID, cursor); err != nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
		}
		binding.ReplayHorizonEventID = meta.ReplayHorizonEventID
		binding.PullCursorEventID = cursor
		body, err = client.PollProjectEvents(ctx, hubProjectID, binding.PullCursorEventID, federationPollLimit)
		if err != nil {
			return recordFederationSyncError(ctx, store, binding.ProjectID, err)
		}
		if body.ResetRequired {
			return recordFederationSyncError(ctx, store, binding.ProjectID, ErrFederationResetRequired)
		}
		if err := store.RecordFederationSyncReset(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
			return err
		}
	}
	var pulledEvents []db.Event
	if err := store.RetryTransient(ctx, func() error {
		currentBinding, err := store.FederationBindingByProject(ctx, binding.ProjectID)
		if err != nil {
			return err
		}
		shouldDeliverPage := len(body.Events) > 0 && body.NextAfterID > currentBinding.PullCursorEventID
		deliverUIDs := make([]string, 0, len(body.Events))
		localInstanceUID := store.InstanceUID()
		for _, ev := range body.Events {
			inserted, err := store.InsertRemoteEvent(ctx, binding.ProjectID, remoteEventFromEnvelope(ev))
			if err != nil {
				return err
			}
			deliverDuplicate := false
			if !inserted && shouldDeliverPage {
				deliverDuplicate, err = shouldDeliverDuplicatePulledEvent(ctx, store, currentBinding, runStartBinding, ev, localInstanceUID)
				if err != nil {
					return err
				}
			}
			if inserted || deliverDuplicate {
				deliverUIDs = append(deliverUIDs, ev.EventUID)
			}
		}
		if len(body.Events) > 0 {
			if err := federationFailpoint("during_spoke_pull_apply_before_materialize"); err != nil {
				return err
			}
			if err := store.MaterializeFederatedProject(ctx, binding.ProjectID); err != nil {
				return err
			}
		}
		if shouldDeliverPage && len(deliverUIDs) > 0 {
			events, err := store.EventsByUIDs(ctx, binding.ProjectID, deliverUIDs)
			if err != nil {
				return err
			}
			pulledEvents = events
		}
		return store.AdvanceFederationPullCursor(ctx, binding.ProjectID, body.NextAfterID)
	}); err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	if onPulledEvents != nil && len(pulledEvents) > 0 {
		onPulledEvents(binding.ProjectID, pulledEvents)
	}
	return store.RecordFederationSyncPullSuccess(ctx, binding.ProjectID, time.Now().UTC())
}

func shouldDeliverDuplicatePulledEvent(
	ctx context.Context,
	store db.Storage,
	binding db.FederationBinding,
	runStartBinding db.FederationBinding,
	ev api.EventEnvelope,
	localInstanceUID string,
) (bool, error) {
	if ev.OriginInstanceUID != localInstanceUID {
		return true, nil
	}
	events, err := store.EventsByUIDs(ctx, binding.ProjectID, []string{ev.EventUID})
	if err != nil {
		return false, err
	}
	if len(events) != 1 {
		return false, nil
	}
	event := events[0]
	if event.ID > binding.PushCursorEventID {
		return true, nil
	}
	if event.IssueID == nil && event.IssueUID != nil {
		return true, nil
	}
	if event.ID <= runStartBinding.PushCursorEventID {
		return false, nil
	}
	if runStartBinding.PullCursorEventID != runStartBinding.ReplayHorizonEventID-1 {
		return false, nil
	}
	status, err := store.FederationSyncStatusByProject(ctx, binding.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status.LastResetAt == nil {
		return false, nil
	}
	if !event.CreatedAt.Before(*status.LastResetAt) {
		return false, nil
	}
	if status.LastPullSuccessAt != nil && !status.LastPullSuccessAt.Before(*status.LastResetAt) {
		return false, nil
	}
	return true, nil
}

func federationIngestEnvelopes(events []db.Event) []api.FederationIngestEventEnvelope {
	out := make([]api.FederationIngestEventEnvelope, 0, len(events))
	for _, ev := range events {
		out = append(out, api.FederationIngestEventEnvelope{
			EventID:           ev.ID,
			EventUID:          ev.UID,
			OriginInstanceUID: ev.OriginInstanceUID,
			ProjectUID:        ev.ProjectUID,
			ProjectName:       ev.ProjectName,
			IssueUID:          ev.IssueUID,
			RelatedIssueUID:   ev.RelatedIssueUID,
			Type:              ev.Type,
			Actor:             ev.Actor,
			HLCPhysicalMS:     ev.HLCPhysicalMS,
			HLCCounter:        ev.HLCCounter,
			ContentHash:       ev.ContentHash,
			Payload:           json.RawMessage(ev.Payload),
			CreatedAt:         ev.CreatedAt,
		})
	}
	return out
}

func isPoisonedFederationPushError(err error) bool {
	var statusErr *HubStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.StatusCode == http.StatusBadRequest || statusErr.StatusCode == http.StatusConflict
}

func recordFederationPushQuarantine(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	events []db.Event,
	syncErr error,
) error {
	if len(events) == 0 {
		return nil
	}
	uids := make([]string, 0, len(events))
	for _, ev := range events {
		uids = append(uids, ev.UID)
	}
	_, err := store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    projectID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: events[0].ID,
		LastEventID:  events[len(events)-1].ID,
		EventUIDs:    uids,
		Error:        syncErr.Error(),
		CreatedAt:    time.Now().UTC(),
	})
	return err
}

func remoteEventFromEnvelope(ev api.EventEnvelope) db.RemoteEvent {
	return db.RemoteEvent{
		EventUID:          ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		ContentHash:       ev.ContentHash,
		Payload:           ev.Payload,
		CreatedAt:         ev.CreatedAt,
	}
}

// Runner quietly pulls every enabled spoke binding.
type Runner struct {
	DB             db.Storage
	Opts           clientpkg.Opts
	Interval       time.Duration
	Wake           <-chan struct{}
	Debounce       time.Duration
	OnError        func(error)
	OnPulledEvents func(projectID int64, events []db.Event)
}

func (r *Runner) clientOpts() clientpkg.Opts {
	return clientOptsWithDefault(r.Opts)
}

func clientOptsWithDefault(opts clientpkg.Opts) clientpkg.Opts {
	if opts.Timeout == 0 {
		opts.Timeout = defaultClientTimeout
	}
	return opts
}

type activeSpokeBinding struct {
	binding db.FederationBinding
	project db.Project
}

// RunOnce executes one pull pass. With no spoke bindings it returns without
// reading credentials or making network requests.
func (r *Runner) RunOnce(ctx context.Context) error {
	bindings, err := r.DB.ListFederationBindings(ctx)
	if err != nil {
		return err
	}
	spokes := make([]activeSpokeBinding, 0, len(bindings))
	for _, binding := range bindings {
		if !binding.Enabled || binding.Role != db.FederationRoleSpoke {
			continue
		}
		project, err := r.DB.ProjectByID(ctx, binding.ProjectID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return err
		}
		if project.DeletedAt != nil {
			continue
		}
		spokes = append(spokes, activeSpokeBinding{binding: binding, project: project})
	}
	if len(spokes) == 0 {
		return nil
	}
	creds, err := config.ReadFederationCredentials()
	if err != nil {
		var errs []error
		errs = append(errs, err)
		for _, spoke := range spokes {
			binding := spoke.binding
			if recordErr := r.DB.RecordFederationSyncError(ctx, binding.ProjectID, err, time.Now().UTC()); recordErr != nil {
				errs = append(errs, recordErr)
			}
		}
		return errors.Join(errs...)
	}
	var errs []error
	for _, spoke := range spokes {
		binding := spoke.binding
		bindingErrs := len(errs)
		project := spoke.project
		cred := creds.Projects[project.UID]
		if cred.HubURL == "" {
			cred.HubURL = binding.HubURL
		}
		if cred.HubProjectID == 0 {
			cred.HubProjectID = binding.HubProjectID
		}
		opts := r.clientOpts()
		if err := RetryPendingClaimsOnce(ctx, r.DB, binding, cred, opts); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			errs = append(errs, err)
		}
		if err := SyncFederationOnceWithPulledEvents(ctx, r.DB, binding, cred, opts, r.OnPulledEvents); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			errs = append(errs, err)
		}
		if len(errs) == bindingErrs {
			if err := r.DB.ClearFederationSyncError(ctx, binding.ProjectID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// RetryPendingClaimsOnce retries offline spoke claim requests against their
// authoritative hub.
func RetryPendingClaimsOnce(
	ctx context.Context,
	store db.Storage,
	binding db.FederationBinding,
	creds config.FederationCredential,
	opts clientpkg.Opts,
) error {
	if !binding.Enabled || binding.Role != db.FederationRoleSpoke || strings.TrimSpace(creds.Token) == "" {
		return nil
	}
	pending, err := store.ListPendingClaimRequests(ctx, binding.ProjectID, federationPollLimit)
	if err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	if len(pending) == 0 {
		return nil
	}
	if err := store.RecordFederationSyncPushStarted(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
		return err
	}
	hasKnownCapabilities := strings.TrimSpace(creds.Capabilities) != ""
	if hasKnownCapabilities && !federationCredentialHasCapability(creds.Capabilities, "claim") {
		now := time.Now().UTC()
		for _, req := range pending {
			if err := store.RejectPendingClaim(ctx, req.RequestUID, "lease capability unavailable", now); err != nil {
				return recordFederationSyncError(ctx, store, binding.ProjectID, err)
			}
		}
		return store.RecordFederationSyncPushSuccess(ctx, binding.ProjectID, time.Now().UTC())
	}
	hubURL := creds.HubURL
	if hubURL == "" {
		hubURL = binding.HubURL
	}
	hubProjectID := creds.HubProjectID
	if hubProjectID == 0 {
		hubProjectID = binding.HubProjectID
	}
	client, err := NewClient(ctx, hubURL, creds.Token, clientOptsWithDefault(opts))
	if err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	var errs []error
	for _, req := range pending {
		if err := retryPendingClaim(ctx, store, client, hubProjectID, req); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	return store.RecordFederationSyncPushSuccess(ctx, binding.ProjectID, time.Now().UTC())
}

func recordFederationSyncError(ctx context.Context, store db.Storage, projectID int64, syncErr error) error {
	if syncErr == nil {
		return nil
	}
	if err := store.RecordFederationSyncError(ctx, projectID, syncErr, time.Now().UTC()); err != nil {
		return errors.Join(syncErr, err)
	}
	return syncErr
}

func retryPendingClaim(
	ctx context.Context,
	store db.Storage,
	client *Client,
	hubProjectID int64,
	pending db.PendingClaimRequest,
) error {
	req := ClaimRequest{
		Holder:     pending.Holder,
		ClientKind: pending.ClientKind,
		ClaimKind:  pending.ClaimKind,
		Purpose:    pending.Purpose,
	}
	if pending.TTLSeconds != nil {
		req.TTLSeconds = *pending.TTLSeconds
	}
	resp, err := client.AcquireClaim(ctx, hubProjectID, pending.IssueUID, req)
	now := time.Now().UTC()
	if err != nil {
		var statusErr *HubStatusError
		if errors.As(err, &statusErr) {
			if statusErr.StatusCode == http.StatusForbidden || statusErr.StatusCode == http.StatusConflict {
				return store.RejectPendingClaim(ctx, pending.RequestUID, statusErr.Error(), now)
			}
		}
		if markErr := store.MarkPendingClaimAttempt(ctx, pending.RequestUID, err.Error(), now); markErr != nil {
			return markErr
		}
		return err
	}
	lease := resp.Lease
	if lease == nil {
		lease = resp.Claim
	}
	if resp.Granted && lease != nil {
		return store.ResolvePendingClaim(ctx, pending.RequestUID, issueClaimFromAPI(lease))
	}
	return store.RejectPendingClaim(ctx, pending.RequestUID, "lease denied by hub", now)
}

func federationCredentialHasCapability(capabilities, capability string) bool {
	for _, part := range strings.Split(capabilities, ",") {
		if strings.TrimSpace(part) == capability {
			return true
		}
	}
	return false
}

func issueClaimFromAPI(claim *api.IssueClaimOut) db.IssueClaim {
	if claim == nil {
		return db.IssueClaim{}
	}
	return db.IssueClaim{
		ClaimUID:          claim.ClaimUID,
		ProjectID:         claim.ProjectID,
		IssueUID:          claim.IssueUID,
		Holder:            claim.Holder,
		HolderInstanceUID: claim.HolderInstanceUID,
		ClientKind:        claim.ClientKind,
		Purpose:           claim.Purpose,
		ClaimKind:         claim.ClaimKind,
		AcquiredAt:        claim.AcquiredAt,
		ExpiresAt:         claim.ExpiresAt,
		ReleasedAt:        claim.ReleasedAt,
		ReleaseReason:     claim.ReleaseReason,
		Revision:          claim.Revision,
		UpdatedAt:         claim.UpdatedAt,
	}
}

// Run executes pull passes until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	debounce := r.Debounce
	if debounce <= 0 {
		debounce = 50 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.RunOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			if r.OnError != nil {
				r.OnError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-r.Wake:
			timer := time.NewTimer(debounce)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}
