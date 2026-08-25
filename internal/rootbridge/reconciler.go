package rootbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/connector"
)

const (
	defaultExternalRootClaimStaleAfter = db.ExternalRootClaimStaleAfter
	externalRootFinalizationTimeout    = 10 * time.Second
)

// ReconcilerConfig supplies clock, claim, and retry policies.
type ReconcilerConfig struct {
	Now             func() time.Time
	ClaimStaleAfter time.Duration
	RetryAt         func(db.ExternalRootBinding, time.Time) time.Time
}

// Reconciler synchronizes one durable external-root binding at a time.
type Reconciler struct {
	store           db.Storage
	registry        *Registry
	now             func() time.Time
	claimStaleAfter time.Duration
	retryAt         func(db.ExternalRootBinding, time.Time) time.Time
}

type externalRootClaimLease struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   <-chan error
	once   sync.Once
	err    error
}

func (l *externalRootClaimLease) stop() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.cancel(nil)
		l.err = <-l.done
	})
	return l.err
}

// RunResult summarizes one reconciliation and retains its committed events.
type RunResult struct {
	RootUpdated        bool       `json:"root_updated"`
	CommentsCreated    int        `json:"comments_created"`
	CommentsEdited     int        `json:"comments_edited"`
	FieldConflicts     int        `json:"field_conflicts"`
	CompletionRequests int        `json:"completion_requests"`
	ReopenRequests     int        `json:"reopen_requests"`
	Paused             bool       `json:"paused"`
	Events             []db.Event `json:"events,omitempty"`
	eventSink          func(db.Event)
}

func (r *RunResult) retainEvents(events ...db.Event) {
	for _, event := range events {
		if event.ID == 0 {
			continue
		}
		if r.eventSink != nil {
			r.eventSink(event)
		}
		r.Events = append(r.Events, event)
	}
}

type reconcileSnapshot struct {
	binding                  db.ExternalRootBinding
	issue                    db.Issue
	description              connector.Description
	client                   connectorclient.Client
	root                     connector.Root
	comments                 []connector.Comment
	kataComments             []db.Comment
	mappings                 []db.ImportMapping
	publishedCommentMappings []db.ImportMapping
	commentIdentityFrontier  bool
	seenCommentRevisions     map[string]bool
	mappedComments           map[int64]bool
	fieldMappings            []db.ExternalFieldMapping
	fieldStates              []db.ExternalFieldState
	fieldIssue               db.Issue
	coordinatedTimezone      string
	publishReady             bool
}

type pauseExternalRootError struct {
	reason string
	cause  error
}

func (e *pauseExternalRootError) Error() string { return e.cause.Error() }
func (e *pauseExternalRootError) Unwrap() error { return e.cause }

// NewReconciler constructs an external-root reconciler.
func NewReconciler(store db.Storage, registry *Registry, config ReconcilerConfig) *Reconciler {
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	claimStaleAfter := config.ClaimStaleAfter
	if claimStaleAfter <= 0 {
		claimStaleAfter = defaultExternalRootClaimStaleAfter
	}
	retryAt := config.RetryAt
	if retryAt == nil {
		retryAt = func(_ db.ExternalRootBinding, now time.Time) time.Time { return now }
	}
	return &Reconciler{store: store, registry: registry, now: now, claimStaleAfter: claimStaleAfter, retryAt: retryAt}
}

// RunOptions selects the claim path and optional immediate event delivery for
// one reconciliation. The zero value acquires the ordinary due-time claim.
type RunOptions struct {
	Manual     bool
	ClaimToken string
	EventSink  func(db.Event)
}

// Run reconciles one binding through the claim path selected by options.
// Manual bypasses only the durable due time. ClaimToken enters with a claim
// already committed by the caller, as required by the bind path.
func (r *Reconciler) Run(ctx context.Context, bindingID int64, options RunOptions) (RunResult, error) {
	if strings.TrimSpace(options.ClaimToken) != "" {
		return r.runClaimedByID(ctx, bindingID, options.ClaimToken, options.EventSink)
	}
	if options.Manual {
		return r.runManual(ctx, bindingID, options.EventSink)
	}
	return r.runOnce(ctx, bindingID, options.EventSink)
}

func (r *Reconciler) runOnce(
	ctx context.Context,
	bindingID int64,
	eventSink func(db.Event),
) (RunResult, error) {
	if r == nil || r.store == nil {
		return RunResult{}, errors.New("external root reconciler storage is unavailable")
	}
	token, err := katauid.New()
	if err != nil {
		return RunResult{}, fmt.Errorf("generate external root claim token: %w", err)
	}
	now := r.timestamp()
	binding, claimed, err := r.store.ClaimExternalRootBinding(
		ctx, bindingID, token, now, now.Add(-r.claimStaleAfter),
	)
	if err != nil || !claimed {
		return RunResult{}, err
	}
	return r.runClaimed(ctx, binding, token, eventSink)
}

func (r *Reconciler) runManual(
	ctx context.Context,
	bindingID int64,
	eventSink func(db.Event),
) (RunResult, error) {
	if r == nil || r.store == nil {
		return RunResult{}, errors.New("external root reconciler storage is unavailable")
	}
	token, err := katauid.New()
	if err != nil {
		return RunResult{}, fmt.Errorf("generate external root claim token: %w", err)
	}
	now := r.timestamp()
	binding, claimed, err := r.store.ClaimExternalRootBindingForManualReconcile(
		ctx, bindingID, token, now, now.Add(-r.claimStaleAfter),
	)
	if err != nil || !claimed {
		if err == nil {
			err = db.ErrExternalRootClaimLost
		}
		return RunResult{}, err
	}
	return r.runClaimed(ctx, binding, token, eventSink)
}

func (r *Reconciler) runClaimedByID(
	ctx context.Context,
	bindingID int64,
	claimToken string,
	eventSink func(db.Event),
) (RunResult, error) {
	if r == nil || r.store == nil {
		return RunResult{}, errors.New("external root reconciler storage is unavailable")
	}
	validationCtx, validationCancel := r.finalizationContext(ctx)
	binding, err := r.store.ExternalRootBindingByID(validationCtx, bindingID)
	validationCancel()
	if err != nil {
		return RunResult{}, err
	}
	if strings.TrimSpace(claimToken) == "" || binding.ClaimToken != claimToken || !binding.Active || !binding.Enabled {
		return RunResult{}, db.ErrExternalRootClaimLost
	}
	return r.runClaimed(ctx, binding, claimToken, eventSink)
}

func (r *Reconciler) runClaimed(
	ctx context.Context,
	binding db.ExternalRootBinding,
	claimToken string,
	eventSink func(db.Event),
) (result RunResult, returnErr error) {
	result.eventSink = eventSink
	parentCtx := ctx
	claimHeld := true
	var lease *externalRootClaimLease
	defer func() {
		if lease != nil {
			_ = lease.stop()
		}
		if recovered := recover(); recovered != nil {
			panicErr := errors.New("external connector panicked")
			if leaseErr := lease.stop(); leaseErr != nil {
				panicErr = errors.Join(panicErr, leaseErr)
			}
			returnErr = errors.Join(returnErr, r.recordFailure(parentCtx, binding, claimToken, panicErr, nil, &claimHeld))
		}
		if !claimHeld {
			return
		}
		finalizeCtx, finalizeCancel := r.finalizationContext(parentCtx)
		defer finalizeCancel()
		_, releaseErr := r.store.ReleaseExternalRootClaim(finalizeCtx, binding.ID, claimToken)
		if releaseErr != nil && !errors.Is(releaseErr, db.ErrExternalRootClaimLost) {
			returnErr = errors.Join(returnErr, fmt.Errorf("release external root claim: %w", releaseErr))
		}
	}()
	var err error
	lease, err = r.startClaimLease(ctx, binding.ID, claimToken)
	if err != nil {
		return result, r.recordFailure(parentCtx, binding, claimToken, err, nil, &claimHeld)
	}
	ctx = lease.ctx
	finishLease := func(cause error) error {
		if leaseErr := lease.stop(); leaseErr != nil {
			return errors.Join(cause, leaseErr)
		}
		return cause
	}
	fail := func(cause error, verifiedRoot *connector.Root) error {
		return r.recordFailure(parentCtx, binding, claimToken, finishLease(cause), verifiedRoot, &claimHeld)
	}

	snapshot, err := r.readCurrent(ctx, binding, claimToken)
	if err != nil {
		if pauseErr, ok := errors.AsType[*pauseExternalRootError](err); ok {
			err = finishLease(err)
			finalizeCtx, finalizeCancel := r.finalizationContext(parentCtx)
			_, event, pauseWriteErr := r.store.PauseExternalRootBinding(finalizeCtx, db.ExternalRootActionParams{
				BindingID: binding.ID, ClaimToken: claimToken,
				Actor: integrationActor(binding), Reason: pauseErr.reason,
			})
			finalizeCancel()
			if pauseWriteErr != nil {
				return result, r.recordFailure(parentCtx, binding, claimToken, errors.Join(err, pauseWriteErr), nil, &claimHeld)
			}
			claimHeld = false
			result.Paused = true
			result.retainEvents(event)
			return result, nil
		}
		return result, fail(err, nil)
	}

	result, err = r.applyInbound(ctx, snapshot, claimToken, result)
	if err != nil {
		return result, fail(err, nil)
	}
	result, err = r.applyFields(ctx, &snapshot, claimToken, result)
	if err != nil {
		return result, fail(err, nil)
	}
	result, outboundErr := r.applyOutboundComments(ctx, snapshot, claimToken, result)
	if outboundErr != nil {
		return result, fail(outboundErr, nil)
	}
	result, completionErr := r.completeExternal(ctx, &snapshot, claimToken, result)
	if completionErr != nil {
		var verifiedRoot *connector.Root
		if externalRootComplete(snapshot.root.State) {
			verifiedRoot = &snapshot.root
		}
		return result, fail(completionErr, verifiedRoot)
	}
	if leaseErr := finishLease(nil); leaseErr != nil {
		return result, r.recordFailure(parentCtx, binding, claimToken, leaseErr, &snapshot.root, &claimHeld)
	}
	now := r.timestamp()
	finalizeCtx, finalizeCancel := r.finalizationContext(parentCtx)
	_, err = r.store.RecordExternalRootSuccess(finalizeCtx, db.ExternalRootSuccessParams{
		BindingID: binding.ID, ClaimToken: claimToken, At: now, NextAttemptAt: now,
		ExternalState: snapshot.root.State, ExternalRevision: snapshot.root.Revision,
	})
	finalizeCancel()
	if err != nil {
		return result, r.recordFailure(parentCtx, binding, claimToken, err, &snapshot.root, &claimHeld)
	}
	claimHeld = false
	return result, nil
}

func (r *Reconciler) startClaimLease(
	ctx context.Context,
	bindingID int64,
	claimToken string,
) (*externalRootClaimLease, error) {
	return startExternalRootClaimLease(
		ctx, r.store, bindingID, claimToken, r.claimStaleAfter, r.timestamp,
	)
}

func startExternalRootClaimLease(
	ctx context.Context,
	store db.Storage,
	bindingID int64,
	claimToken string,
	staleAfter time.Duration,
	now func() time.Time,
) (*externalRootClaimLease, error) {
	if _, err := store.RenewExternalRootClaim(ctx, bindingID, claimToken, now()); err != nil {
		return nil, err
	}
	leaseCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan error, 1)
	interval := staleAfter / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				if _, err := store.RenewExternalRootClaim(
					leaseCtx, bindingID, claimToken, now(),
				); err != nil {
					if leaseCtx.Err() != nil {
						done <- nil
						return
					}
					renewErr := fmt.Errorf("renew external root claim: %w", err)
					cancel(renewErr)
					done <- renewErr
					return
				}
			}
		}
	}()
	return &externalRootClaimLease{ctx: leaseCtx, cancel: cancel, done: done}, nil
}

func (r *Reconciler) renewBeforeExternalCall(
	ctx context.Context,
	bindingID int64,
	claimToken string,
) error {
	return renewExternalRootClaimBeforeCall(ctx, r.store, bindingID, claimToken, r.timestamp())
}

func renewExternalRootClaimBeforeCall(
	ctx context.Context,
	store db.Storage,
	bindingID int64,
	claimToken string,
	now time.Time,
) error {
	_, err := store.RenewExternalRootClaim(ctx, bindingID, claimToken, now)
	return err
}

func (r *Reconciler) completeExternal(
	ctx context.Context,
	snapshot *reconcileSnapshot,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	if snapshot.issue.Status != "closed" || !snapshot.binding.CompleteExternal || externalRootComplete(snapshot.root.State) {
		return result, nil
	}
	issue, err := r.store.IssueByID(ctx, snapshot.binding.IssueID)
	if err != nil {
		return result, err
	}
	if issue.Status != "closed" {
		return result, nil
	}
	binding, err := r.store.ExternalRootBindingByID(ctx, snapshot.binding.ID)
	if err != nil {
		return result, err
	}
	if !sameExternalRootCompletionAuthority(snapshot.binding, binding, claimToken) {
		return result, db.ErrExternalRootClaimLost
	}
	if !binding.CompleteExternal {
		return result, nil
	}
	if err := r.renewBeforeExternalCall(ctx, binding.ID, claimToken); err != nil {
		return result, err
	}
	if _, err := snapshot.client.CompleteRoot(ctx, connector.CompleteRootParams{
		RootKey: binding.ExternalRootKey,
	}); err != nil {
		return result, err
	}
	readback, err := snapshot.client.ReadRoot(ctx, connector.ReadRootParams{
		RootKey: binding.ExternalRootKey,
	})
	if err != nil {
		return result, err
	}
	if readback.Key != snapshot.binding.ExternalRootKey ||
		readback.IdentityKey != snapshot.binding.ExternalAccountKey {
		return result, fmt.Errorf("%w: external completion readback identity changed", db.ErrExternalRootValidation)
	}
	if !externalRootComplete(readback.State) {
		return result, fmt.Errorf("%w: external completion could not be verified", db.ErrExternalRootValidation)
	}
	if strings.TrimSpace(readback.Revision) == "" || readback.UpdatedAt.IsZero() || readback.ObservedAt.IsZero() {
		return result, fmt.Errorf("%w: external completion readback revision and timestamps are required", db.ErrExternalRootValidation)
	}
	snapshot.root = readback
	return result, nil
}

func sameExternalRootCompletionAuthority(
	expected db.ExternalRootBinding,
	current db.ExternalRootBinding,
	claimToken string,
) bool {
	return current.ID == expected.ID &&
		current.UID == expected.UID &&
		current.ProjectID == expected.ProjectID &&
		current.IssueID == expected.IssueID &&
		current.RootMappingID == expected.RootMappingID &&
		current.ConnectorInstance == expected.ConnectorInstance &&
		current.ExternalRootKey == expected.ExternalRootKey &&
		current.ExternalAccountKey == expected.ExternalAccountKey &&
		current.Active && current.Enabled && current.ClaimToken == claimToken
}

func (r *Reconciler) applyFields(
	ctx context.Context,
	snapshot *reconcileSnapshot,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	active := make([]db.ExternalFieldMapping, 0, len(snapshot.fieldMappings))
	for _, mapping := range snapshot.fieldMappings {
		if mapping.Active {
			active = append(active, mapping)
		}
	}
	if len(active) == 0 {
		return result, nil
	}
	if !descriptionSupportsFields(snapshot.description) {
		return result, ErrFieldSynchronizationUnavailable
	}
	listed, err := snapshot.client.ListFields(ctx)
	if err != nil {
		return result, err
	}
	states := make(map[int64]db.ExternalFieldState, len(snapshot.fieldStates))
	for _, state := range snapshot.fieldStates {
		states[state.MappingID] = state
	}
	snapshot.fieldIssue = snapshot.issue
	snapshot.coordinatedTimezone = ""
	for _, mapping := range active {
		state, hasState := states[mapping.ID]
		result, err = r.reconcileField(ctx, snapshot, listed.Fields, mapping, state, hasState, claimToken, result)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (r *Reconciler) reconcileField(
	ctx context.Context,
	snapshot *reconcileSnapshot,
	liveFields []connector.FieldDescriptor,
	mapping db.ExternalFieldMapping,
	state db.ExternalFieldState,
	hasState bool,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	baseline, hasBaseline, baselineErr := decodeCanonicalStoredFieldValue(state.Baseline, mapping)
	if baselineErr != nil {
		hasBaseline = false
		baseline = nullFieldValue()
	}
	codec, ok := fieldCodecs[mapping.KataField]
	if !ok {
		return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, nullFieldValue(), nullFieldValue(), claimToken, result)
	}
	if _, err := validateLiveFieldDescriptor(codec, mapping, liveFields); err != nil {
		kata := baseline
		if current, readErr := readCanonicalKataField(codec, snapshot.issue, mapping); readErr == nil {
			kata = current
		}
		return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, kata, baseline, claimToken, result)
	}

	fieldIssue := snapshot.issue
	if snapshot.coordinatedTimezone != "" {
		fieldIssue = snapshot.fieldIssue
	}
	kata, err := readCanonicalKataField(codec, fieldIssue, mapping)
	if err != nil {
		external := baseline
		if current, readErr := readMappedExternalField(ctx, snapshot, mapping); readErr == nil {
			external = current
		} else if !isInvalidExternalFieldRead(readErr) {
			return result, readErr
		}
		return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, baseline, external, claimToken, result)
	}
	external, observedExternal, err := readMappedExternalFieldObserved(ctx, snapshot, mapping)
	if err != nil {
		if !isInvalidExternalFieldRead(err) {
			return result, err
		}
		return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, kata, baseline, claimToken, result)
	}
	if baselineErr != nil {
		return r.storeFieldConflict(ctx, snapshot, mapping, state, false, kata, external, claimToken, result)
	}
	if hasState && state.Conflicted {
		return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, kata, external, claimToken, result)
	}

	var decision MergeDecision
	if hasState && hasBaseline {
		decision = DecideFieldMerge(baseline, kata, external)
	} else {
		decision = DecideInitialFieldMerge(kata, external)
	}
	switch decision.Action {
	case MergeConflict:
		return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, kata, external, claimToken, result)
	case MergeWriteKata:
		coordinatedTimezone := snapshot.coordinatedTimezone
		if coordinatedTimezone == "" {
			coordinatedTimezone = r.coordinatedTimezoneForField(
				ctx, snapshot, liveFields, mapping, decision, external,
			)
			if coordinatedTimezone != "" {
				snapshot.coordinatedTimezone = coordinatedTimezone
			}
		}
		patch, patchErr := kataPatchWithCoordinatedTimezone(
			codec, snapshot.issue, decision.Baseline, coordinatedTimezone,
		)
		if patchErr != nil {
			return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, kata, external, claimToken, result)
		}
		projected, event, _, projectionErr := r.store.ApplyExternalFieldProjection(ctx, db.ExternalFieldProjectionParams{
			BindingID: snapshot.binding.ID, MappingID: mapping.ID, ClaimToken: claimToken,
			KataField: mapping.KataField, Patch: patch, ExpectedIssueRevision: snapshot.issue.Revision,
			IntegrationActor: integrationActor(snapshot.binding),
		})
		if projectionErr != nil {
			return result, projectionErr
		}
		if event != nil {
			result.retainEvents(*event)
		}
		snapshot.issue = projected
		readback, readErr := readCanonicalKataField(codec, snapshot.issue, mapping)
		if readErr != nil || !fieldValuesEqual(readback, decision.Baseline) {
			if readErr != nil {
				readback = kata
			}
			return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, readback, external, claimToken, result)
		}
	case MergeWriteExternal:
		if err := r.renewBeforeExternalCall(ctx, snapshot.binding.ID, claimToken); err != nil {
			return result, err
		}
		_, writeErr := snapshot.client.WriteFields(ctx, connector.WriteFieldsParams{
			RootKey:  snapshot.binding.ExternalRootKey,
			Fields:   map[string]connector.FieldValue{mapping.ExternalFieldID: decision.Baseline},
			Expected: map[string]connector.FieldValue{mapping.ExternalFieldID: observedExternal},
		})
		if writeErr != nil {
			return result, writeErr
		}
		readback, readErr := readMappedExternalField(ctx, snapshot, mapping)
		if readErr != nil {
			if !isInvalidExternalFieldRead(readErr) {
				return result, readErr
			}
			return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, kata, external, claimToken, result)
		}
		if !fieldValuesEqual(readback, decision.Baseline) {
			return r.storeFieldConflict(ctx, snapshot, mapping, state, hasBaseline, kata, readback, claimToken, result)
		}
	case MergeAccept:
	default:
		return result, fmt.Errorf("unsupported field merge action %q", decision.Action)
	}

	baselineJSON, err := marshalCanonicalFieldValue(decision.Baseline)
	if err != nil {
		return result, err
	}
	_, event, err := r.store.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
		BindingID: snapshot.binding.ID, MappingID: mapping.ID, ClaimToken: claimToken,
		Baseline: baselineJSON, At: r.timestamp(), Actor: integrationActor(snapshot.binding),
	})
	if err != nil {
		return result, err
	}
	if event != nil {
		result.retainEvents(*event)
	}
	return result, nil
}

func (r *Reconciler) coordinatedTimezoneForField(
	ctx context.Context,
	snapshot *reconcileSnapshot,
	liveFields []connector.FieldDescriptor,
	mapping db.ExternalFieldMapping,
	decision MergeDecision,
	external connector.FieldValue,
) string {
	if decision.Action != MergeWriteKata || external.Kind != fieldKindLocalDateTime || external.Timezone == "" {
		return ""
	}
	currentCodec, ok := fieldCodecs[mapping.KataField]
	if !ok {
		return ""
	}
	currentKata, err := readCanonicalKataField(currentCodec, snapshot.issue, mapping)
	if err != nil || currentKata.Kind != fieldKindLocalDateTime || currentKata.Timezone == external.Timezone {
		return ""
	}
	states := make(map[int64]db.ExternalFieldState, len(snapshot.fieldStates))
	for _, state := range snapshot.fieldStates {
		states[state.MappingID] = state
	}
	foundSibling := false
	for _, sibling := range snapshot.fieldMappings {
		if !sibling.Active || sibling.ID == mapping.ID {
			continue
		}
		codec, ok := fieldCodecs[sibling.KataField]
		if !ok {
			return ""
		}
		siblingKata, err := readCanonicalKataField(codec, snapshot.issue, sibling)
		if err != nil || siblingKata.Kind != fieldKindLocalDateTime || siblingKata.Timezone != currentKata.Timezone {
			return ""
		}
		if _, err := validateLiveFieldDescriptor(codec, sibling, liveFields); err != nil {
			return ""
		}
		siblingExternal, err := readMappedExternalField(ctx, snapshot, sibling)
		if err != nil || siblingExternal.Kind != fieldKindLocalDateTime || siblingExternal.Timezone != external.Timezone {
			return ""
		}
		state, ok := states[sibling.ID]
		if !ok || state.Conflicted {
			return ""
		}
		baseline, hasBaseline, err := decodeCanonicalStoredFieldValue(state.Baseline, sibling)
		if err != nil || !hasBaseline {
			return ""
		}
		siblingDecision := DecideFieldMerge(baseline, siblingKata, siblingExternal)
		if siblingDecision.Action != MergeWriteKata && siblingDecision.Action != MergeAccept {
			return ""
		}
		foundSibling = true
	}
	if !foundSibling {
		return ""
	}
	return external.Timezone
}

func (r *Reconciler) storeFieldConflict(
	ctx context.Context,
	snapshot *reconcileSnapshot,
	mapping db.ExternalFieldMapping,
	state db.ExternalFieldState,
	hasBaseline bool,
	kata connector.FieldValue,
	external connector.FieldValue,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	kata = canonicalConflictCandidate(kata)
	external = canonicalConflictCandidate(external)
	kataJSON, err := marshalCanonicalFieldValue(kata)
	if err != nil {
		return result, err
	}
	externalJSON, err := marshalCanonicalFieldValue(external)
	if err != nil {
		return result, err
	}
	var baselineJSON json.RawMessage
	if hasBaseline {
		baselineJSON = append(json.RawMessage(nil), state.Baseline...)
	}
	_, event, err := r.store.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
		BindingID: snapshot.binding.ID, MappingID: mapping.ID, ClaimToken: claimToken,
		Baseline: baselineJSON, ConflictKata: kataJSON, ConflictExternal: externalJSON,
		Conflicted: true, At: r.timestamp(), Actor: integrationActor(snapshot.binding),
	})
	if err != nil {
		return result, err
	}
	result.FieldConflicts++
	if event != nil {
		result.retainEvents(*event)
	}
	return result, nil
}

func readCanonicalKataField(
	codec FieldCodec,
	issue db.Issue,
	mapping db.ExternalFieldMapping,
) (connector.FieldValue, error) {
	value, err := codec.ReadKata(issue)
	if err != nil {
		return connector.FieldValue{}, err
	}
	return canonicalMappedFieldValue(value, mapping)
}

func readMappedExternalField(
	ctx context.Context,
	snapshot *reconcileSnapshot,
	mapping db.ExternalFieldMapping,
) (connector.FieldValue, error) {
	canonical, _, err := readMappedExternalFieldObserved(ctx, snapshot, mapping)
	return canonical, err
}

func readMappedExternalFieldObserved(
	ctx context.Context,
	snapshot *reconcileSnapshot,
	mapping db.ExternalFieldMapping,
) (connector.FieldValue, connector.FieldValue, error) {
	read, err := snapshot.client.ReadFields(ctx, connector.ReadFieldsParams{
		RootKey: snapshot.binding.ExternalRootKey, FieldIDs: []string{mapping.ExternalFieldID},
	})
	if err != nil {
		return connector.FieldValue{}, connector.FieldValue{}, err
	}
	if len(read.Fields) != 1 {
		return connector.FieldValue{}, connector.FieldValue{}, &invalidExternalFieldReadError{cause: errors.New("mapped external field read returned the wrong identity")}
	}
	value, ok := read.Fields[mapping.ExternalFieldID]
	if !ok {
		return connector.FieldValue{}, connector.FieldValue{}, &invalidExternalFieldReadError{cause: errors.New("mapped external field read omitted the stable identity")}
	}
	canonical, err := canonicalMappedFieldValue(value, mapping)
	if err != nil {
		return connector.FieldValue{}, connector.FieldValue{}, &invalidExternalFieldReadError{cause: err}
	}
	return canonical, value, nil
}

func canonicalConflictCandidate(value connector.FieldValue) connector.FieldValue {
	canonical, err := canonicalFieldValue(value)
	if err != nil {
		return nullFieldValue()
	}
	return canonical
}

func (r *Reconciler) readCurrent(
	ctx context.Context,
	claimed db.ExternalRootBinding,
	claimToken string,
) (reconcileSnapshot, error) {
	binding, err := r.store.ExternalRootBindingByID(ctx, claimed.ID)
	if err != nil {
		return reconcileSnapshot{}, err
	}
	if binding.ClaimToken != claimToken || !binding.Active || !binding.Enabled {
		return reconcileSnapshot{}, db.ErrExternalRootClaimLost
	}
	snapshot := reconcileSnapshot{binding: binding}
	snapshot.issue, err = r.store.IssueByID(ctx, binding.IssueID)
	if err != nil {
		return snapshot, err
	}
	if r.registry == nil {
		return snapshot, pauseRoot("external_connector_missing", ErrConnectorInstanceNotFound)
	}
	instance, ok := r.registry.instance(binding.ConnectorInstance)
	if !ok {
		return snapshot, pauseRoot("external_connector_missing", ErrConnectorInstanceNotFound)
	}
	description, err := instance.Client.Describe(ctx)
	if err != nil {
		if !parentContextEnded(ctx, err) {
			r.registry.recordDescribeHealthError(binding.ConnectorInstance, err)
		}
		return snapshot, err
	}
	if err := r.registry.acceptDescription(binding.ConnectorInstance, description); err != nil {
		if errors.Is(err, ErrConnectorIdentityChanged) {
			return snapshot, pauseRoot("external_root_identity_changed", err)
		}
		return snapshot, err
	}
	if description.AccountIdentity != binding.ExternalAccountKey {
		return snapshot, pauseRoot("external_root_identity_changed", ErrConnectorIdentityChanged)
	}
	snapshot.description = description
	snapshot.client = instance.Client
	snapshot.publishReady = descriptionSupportsPublishing(description)
	snapshot.root, err = instance.Client.ReadRoot(ctx, connector.ReadRootParams{RootKey: binding.ExternalRootKey})
	if err != nil {
		if isMissingExternalRoot(err) {
			return snapshot, pauseRoot("external_root_missing", err)
		}
		return snapshot, err
	}
	if strings.EqualFold(strings.TrimSpace(snapshot.root.State), "deleted") {
		return snapshot, pauseRoot("external_root_deleted", errors.New("external root is deleted"))
	}
	if snapshot.root.Key != binding.ExternalRootKey || snapshot.root.IdentityKey != binding.ExternalAccountKey {
		return snapshot, pauseRoot("external_root_identity_changed", ErrConnectorIdentityChanged)
	}
	if strings.TrimSpace(snapshot.root.Revision) == "" || snapshot.root.UpdatedAt.IsZero() || snapshot.root.ObservedAt.IsZero() {
		return snapshot, fmt.Errorf("%w: external root revision and timestamps are required", db.ErrExternalRootValidation)
	}
	listed, err := instance.Client.ListComments(ctx, connector.ListCommentsParams{RootKey: binding.ExternalRootKey})
	if err != nil {
		return snapshot, err
	}
	snapshot.comments = append([]connector.Comment(nil), listed.Comments...)
	snapshot.kataComments, err = r.store.CommentsByIssue(ctx, binding.IssueID)
	if err != nil {
		return snapshot, err
	}
	snapshot.mappings, err = r.store.ImportMappingsByProjectSource(
		ctx, binding.ProjectID, db.ExternalRootCommentMappingSource(binding),
	)
	if err != nil {
		return snapshot, err
	}
	snapshot.publishedCommentMappings, err = r.store.ImportMappingsByProjectSource(
		ctx, binding.ProjectID, db.ExternalRootPublishedCommentMappingSource(binding),
	)
	if err != nil {
		return snapshot, err
	}
	revisionMappings, err := r.store.ImportMappingsByProjectSource(
		ctx, binding.ProjectID, db.ExternalRootCommentRevisionMappingSource(binding),
	)
	if err != nil {
		return snapshot, err
	}
	snapshot.seenCommentRevisions = make(map[string]bool, len(revisionMappings))
	for _, mapping := range revisionMappings {
		if mapping.ObjectType != db.ExternalRevisionAnchorObjectType {
			continue
		}
		if mapping.ExternalID == db.ExternalCommentFrontierExternalID {
			snapshot.commentIdentityFrontier = true
		} else {
			snapshot.seenCommentRevisions[mapping.ExternalID] = true
		}
	}
	snapshot.mappedComments = make(map[int64]bool)
	commentMappings, err := r.store.ImportCommentMappingsByIssue(ctx, binding.IssueID)
	if err != nil {
		return snapshot, err
	}
	for _, mapping := range commentMappings {
		if mapping.CommentID != nil {
			snapshot.mappedComments[*mapping.CommentID] = true
		}
	}
	snapshot.fieldMappings, err = r.store.ListExternalFieldMappings(ctx, binding.ConnectorInstance)
	if err != nil {
		return snapshot, err
	}
	snapshot.fieldStates, err = r.store.ExternalFieldStates(ctx, binding.ID)
	if err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (r *Reconciler) applyInbound(
	ctx context.Context,
	snapshot reconcileSnapshot,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	actor := integrationActor(snapshot.binding)
	_, event, changed, err := r.store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
		BindingID: snapshot.binding.ID, ClaimToken: claimToken,
		Title: snapshot.root.Title, Body: snapshot.root.Body,
		ExternalRevision:  snapshot.root.Revision,
		ExternalUpdatedAt: snapshot.root.UpdatedAt, ExternalObservedAt: snapshot.root.ObservedAt,
		IntegrationActor: actor,
		ExternalActorID:  actorID(snapshot.root.Actor), ExternalActorName: actorName(snapshot.root.Actor),
	})
	if err != nil {
		return result, err
	}
	result.RootUpdated = changed
	if event != nil {
		result.retainEvents(*event)
	}
	result, err = r.applyInboundComments(ctx, snapshot, claimToken, result)
	if err != nil {
		return result, err
	}
	return r.applyLifecycleRequest(ctx, snapshot, claimToken, result)
}

func (r *Reconciler) applyLifecycleRequest(
	ctx context.Context,
	snapshot reconcileSnapshot,
	claimToken string,
	result RunResult,
) (RunResult, error) {
	currentComplete := externalRootComplete(snapshot.root.State)
	previousComplete := externalRootComplete(snapshot.binding.LastExternalState)
	isInitialCompletion := currentComplete && snapshot.binding.LastExternalState == ""
	isCompletionTransition := currentComplete && !previousComplete
	isReopenTransition := !currentComplete && previousComplete
	if !isInitialCompletion && !isCompletionTransition && !isReopenTransition {
		return result, nil
	}

	state := "complete"
	body := completionRequestBody(snapshot.root.Actor)
	if isReopenTransition {
		state = "open"
		body = reopenRequestBody(snapshot.root.Actor)
	}
	providerTime := snapshot.root.UpdatedAt
	_, events, changed, err := r.store.EnsureExternalRootLifecycleRequest(ctx, db.ExternalCommentProjectionParams{
		BindingID: snapshot.binding.ID, ClaimToken: claimToken,
		ExternalID:       "lifecycle:" + snapshot.binding.UID + ":" + state + ":" + snapshot.root.Revision,
		ExternalRevision: snapshot.root.Revision, LifecycleState: state, Body: body,
		ExternalActorID: actorID(snapshot.root.Actor), ExternalActorName: actorName(snapshot.root.Actor),
		ExternalCreatedAt: providerTime, ExternalUpdatedAt: providerTime,
		IntegrationActor: integrationActor(snapshot.binding),
	})
	if err != nil {
		return result, err
	}
	if changed {
		if isReopenTransition {
			result.ReopenRequests++
		} else {
			result.CompletionRequests++
		}
	}
	result.retainEvents(events...)
	return result, nil
}

func (r *Reconciler) recordFailure(
	ctx context.Context,
	binding db.ExternalRootBinding,
	claimToken string,
	cause error,
	verifiedRoot *connector.Root,
	claimHeld *bool,
) error {
	if errors.Is(cause, db.ErrExternalRootClaimLost) {
		return cause
	}
	now := r.timestamp()
	finalizeCtx, finalizeCancel := r.finalizationContext(ctx)
	defer finalizeCancel()
	params := db.ExternalRootErrorParams{
		BindingID: binding.ID, ClaimToken: claimToken, At: now, NextAttemptAt: r.retryAt(binding, now),
		Error: cause.Error(),
	}
	if verifiedRoot != nil && strings.TrimSpace(verifiedRoot.State) != "" &&
		strings.TrimSpace(verifiedRoot.Revision) != "" {
		params.ExternalState = verifiedRoot.State
		params.ExternalRevision = verifiedRoot.Revision
	}
	_, err := r.store.RecordExternalRootError(finalizeCtx, params)
	if err == nil {
		*claimHeld = false
		return cause
	}
	return errors.Join(cause, fmt.Errorf("record external root error: %w", err))
}

func (r *Reconciler) finalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), externalRootFinalizationTimeout)
}

func (r *Reconciler) timestamp() time.Time {
	return r.now().UTC().Truncate(time.Millisecond)
}

func pauseRoot(reason string, cause error) error {
	return &pauseExternalRootError{reason: reason, cause: cause}
}

func isMissingExternalRoot(err error) bool {
	var connectorErr *connector.Error
	if !errors.As(err, &connectorErr) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(connectorErr.Code)) {
	case "not_found", "root_not_found", "deleted", "gone":
		return true
	default:
		return false
	}
}

func integrationActor(binding db.ExternalRootBinding) string {
	return "connector:" + binding.ConnectorInstance
}

func externalRootComplete(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "complete")
}

func actorID(actor *connector.Actor) string {
	if actor == nil {
		return ""
	}
	return actor.ID
}

func actorName(actor *connector.Actor) string {
	if actor == nil {
		return ""
	}
	return actor.DisplayName
}

func completionRequestBody(actor *connector.Actor) string {
	if name := strings.TrimSpace(actorName(actor)); name != "" {
		return fmt.Sprintf("External completion was requested by %s. Please verify completion before closing this Kata issue.", name)
	}
	return "The external root is complete. Please verify completion before closing this Kata issue."
}

func reopenRequestBody(actor *connector.Actor) string {
	if name := strings.TrimSpace(actorName(actor)); name != "" {
		return fmt.Sprintf("External reopening was requested by %s. Please review reopening before changing this Kata issue.", name)
	}
	return "The external root was reopened. Please review reopening before changing this Kata issue."
}
