package rootbridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/connector"
)

var (
	// ErrExternalFieldNotFound reports an unknown external field selector.
	ErrExternalFieldNotFound = errors.New("external field not found")
	// ErrExternalFieldAmbiguous reports a selector that matches multiple fields.
	ErrExternalFieldAmbiguous = errors.New("external field is ambiguous")
	// ErrImmediateReconcileUnavailable reports a missing bind-time reconcile hook.
	ErrImmediateReconcileUnavailable = errors.New("immediate claimed reconciliation is unavailable")
	// ErrCommentPublishingUnavailable reports an instance without safe publishing support.
	ErrCommentPublishingUnavailable = errors.New("connector comment publishing is unavailable")
	// ErrFieldSynchronizationUnavailable reports an instance without field synchronization support.
	ErrFieldSynchronizationUnavailable = errors.New("connector field synchronization is unavailable")
	// ErrPendingCommentResolutionRequired reports a pending publication requiring an operator decision.
	ErrPendingCommentResolutionRequired = errors.New("pending external comment requires explicit resolution")
	// ErrFieldConflictChanged reports candidates that drifted before resolution.
	ErrFieldConflictChanged = errors.New("external field conflict changed")
)

// ImmediateClaimedReconcileFunc is the bind-time handoff to the reconciler.
// The binding is already reserved with claimToken when this function starts;
// implementations use that token to complete one reconciliation before
// returning instead of claiming again.
type ImmediateClaimedReconcileFunc func(context.Context, int64, string) ([]db.Event, error)

// Service coordinates connector validation with durable binding state.
type Service struct {
	store                     db.Storage
	registry                  *Registry
	immediateClaimedReconcile ImmediateClaimedReconcileFunc
	claimStaleAfter           time.Duration
	eventSink                 func(db.Event)
}

// NewService constructs the external-root lifecycle service.
func NewService(store db.Storage, registry *Registry, reconcile ImmediateClaimedReconcileFunc) *Service {
	return NewServiceWithEventSink(store, registry, reconcile, nil)
}

// NewServiceWithEventSink constructs the lifecycle service and delivers each
// committed service event before any later connector work begins.
func NewServiceWithEventSink(
	store db.Storage,
	registry *Registry,
	reconcile ImmediateClaimedReconcileFunc,
	eventSink func(db.Event),
) *Service {
	return &Service{
		store: store, registry: registry, immediateClaimedReconcile: reconcile,
		claimStaleAfter: defaultExternalRootClaimStaleAfter, eventSink: eventSink,
	}
}

func (s *Service) emitEvents(events ...db.Event) {
	if s == nil || s.eventSink == nil {
		return
	}
	for _, event := range events {
		if event.ID != 0 {
			s.eventSink(event)
		}
	}
}

// BindParams selects a Kata issue and external root to bind.
type BindParams struct {
	ProjectID         int64
	IssueID           int64
	ConnectorInstance string
	Locator           string
	Actor             string
	PublishComments   bool
}

// ResolvePendingCommentParams selects an operator action for a pending publish.
type ResolvePendingCommentParams struct {
	IssueID           int64
	Actor             string
	Action            string
	ExternalCommentID string
}

// ResolvePendingComment adopts, retries, or durably skips one pending publish.
func (s *Service) ResolvePendingComment(
	ctx context.Context,
	params ResolvePendingCommentParams,
) (resolved db.ExternalRootBinding, event db.Event, returnErr error) {
	params.Actor = strings.TrimSpace(params.Actor)
	params.Action = strings.TrimSpace(params.Action)
	if params.IssueID <= 0 || params.Actor == "" {
		return resolved, event, fmt.Errorf("%w: issue and actor are required", db.ErrExternalRootValidation)
	}
	switch params.Action {
	case "adopt":
		if strings.TrimSpace(params.ExternalCommentID) == "" {
			return resolved, event, fmt.Errorf("%w: adopt requires an external comment ID", db.ErrExternalRootValidation)
		}
	case "retry", "skip":
		if params.ExternalCommentID != "" {
			return resolved, event, fmt.Errorf("%w: external comment ID is only valid for adopt", db.ErrExternalRootValidation)
		}
	default:
		return resolved, event, fmt.Errorf("%w: unsupported pending comment action", db.ErrExternalRootValidation)
	}
	binding, err := s.store.ExternalRootBindingByIssue(ctx, params.IssueID)
	if err != nil {
		return resolved, event, err
	}
	if !binding.Active || !binding.PublishComments || binding.PendingCommentUID == "" ||
		binding.PendingCommentStartedAt == nil {
		return resolved, event, ErrPendingCommentResolutionRequired
	}
	claimToken, err := katauid.New()
	if err != nil {
		return resolved, event, fmt.Errorf("generate pending comment claim token: %w", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	claimed, acquired, err := s.store.ClaimExternalRootBindingForManualAction(
		ctx, binding.ID, claimToken, now, now.Add(-defaultExternalRootClaimStaleAfter),
	)
	if err != nil {
		return resolved, event, err
	}
	if !acquired {
		return resolved, event, db.ErrExternalRootClaimLost
	}
	parentCtx := ctx
	var lease *externalRootClaimLease
	defer func() {
		if lease != nil {
			if leaseErr := lease.stop(); leaseErr != nil {
				returnErr = errors.Join(returnErr, leaseErr)
			}
		}
		finalizeCtx, finalizeCancel := context.WithTimeout(
			context.WithoutCancel(parentCtx), externalRootFinalizationTimeout,
		)
		defer finalizeCancel()
		released, releaseErr := s.store.ReleaseExternalRootClaim(finalizeCtx, binding.ID, claimToken)
		if releaseErr != nil && !errors.Is(releaseErr, db.ErrExternalRootClaimLost) {
			returnErr = errors.Join(returnErr, fmt.Errorf("release pending comment claim: %w", releaseErr))
			return
		}
		if releaseErr == nil {
			resolved = released
		}
	}()
	lease, err = startExternalRootClaimLease(
		ctx, s.store, claimed.ID, claimToken, s.claimStaleAfter,
		func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) },
	)
	if err != nil {
		return resolved, event, err
	}
	ctx = lease.ctx

	comments, err := s.store.CommentsByIssue(ctx, claimed.IssueID)
	if err != nil {
		return resolved, event, err
	}
	var local db.Comment
	for _, comment := range comments {
		if comment.UID == claimed.PendingCommentUID {
			local = comment
			break
		}
	}
	if local.ID == 0 {
		return resolved, event, ErrPendingCommentResolutionRequired
	}
	var mapping *db.ImportMappingParams
	externalRevision := ""
	if params.Action == "adopt" {
		instance, instanceErr := s.requireInstance(claimed.ConnectorInstance)
		if instanceErr != nil {
			return resolved, event, instanceErr
		}
		description, describeErr := s.describe(ctx, instance)
		if describeErr != nil {
			return resolved, event, describeErr
		}
		if !descriptionSupportsPublishing(description) {
			return resolved, event, ErrCommentPublishingUnavailable
		}
		root, readErr := instance.Client.ReadRoot(ctx, connector.ReadRootParams{RootKey: claimed.ExternalRootKey})
		if readErr != nil {
			return resolved, event, readErr
		}
		if root.Key != claimed.ExternalRootKey || root.IdentityKey != claimed.ExternalAccountKey ||
			description.AccountIdentity != claimed.ExternalAccountKey {
			return resolved, event, fmt.Errorf("%w: account or root does not match binding", ErrConnectorIdentityChanged)
		}
		listed, listErr := instance.Client.ListComments(ctx, connector.ListCommentsParams{RootKey: claimed.ExternalRootKey})
		if listErr != nil {
			return resolved, event, listErr
		}
		var matched *connector.Comment
		for index := range listed.Comments {
			candidate := &listed.Comments[index]
			if candidate.ID != params.ExternalCommentID {
				continue
			}
			if matched != nil || !matchesManualPendingExternalComment(*candidate, description.SelfActorID) {
				return resolved, event, ErrPendingCommentResolutionRequired
			}
			matched = candidate
		}
		if matched == nil {
			return resolved, event, ErrPendingCommentResolutionRequired
		}
		if strings.TrimSpace(matched.Revision) == "" || strings.TrimSpace(matched.Revision) != matched.Revision {
			return resolved, event, ErrPendingCommentResolutionRequired
		}
		built, mappingErr := externalCommentMapping(claimed, local, *matched)
		if mappingErr != nil {
			return resolved, event, ErrPendingCommentResolutionRequired
		}
		mapping = &built
		externalRevision = matched.Revision
	} else if params.Action == "skip" {
		built := skippedCommentMapping(claimed, local)
		mapping = &built
	}
	resolved, event, err = s.store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
		BindingID: claimed.ID, ClaimToken: claimToken, CommentUID: claimed.PendingCommentUID,
		ExpectedBody: local.Body, Action: params.Action, Actor: params.Actor, At: time.Now().UTC(), Mapping: mapping,
		ExternalRevision: externalRevision,
	})
	if err != nil {
		return resolved, event, err
	}
	s.emitEvents(event)
	return resolved, event, nil
}

// Bind validates an external root, creates its binding, and reconciles it once.
func (s *Service) Bind(ctx context.Context, params BindParams) (db.ExternalRootBinding, []db.Event, error) {
	params.ConnectorInstance = strings.TrimSpace(params.ConnectorInstance)
	params.Locator = strings.TrimSpace(params.Locator)
	params.Actor = strings.TrimSpace(params.Actor)
	if params.ProjectID <= 0 || params.IssueID <= 0 || params.ConnectorInstance == "" || params.Locator == "" || params.Actor == "" {
		return db.ExternalRootBinding{}, nil, fmt.Errorf("%w: project, issue, connector, locator, and actor are required", db.ErrExternalRootValidation)
	}
	if s.immediateClaimedReconcile == nil {
		return db.ExternalRootBinding{}, nil, ErrImmediateReconcileUnavailable
	}
	instance, err := s.requireInstance(params.ConnectorInstance)
	if err != nil {
		return db.ExternalRootBinding{}, nil, err
	}
	description, err := s.describe(ctx, instance)
	if err != nil {
		return db.ExternalRootBinding{}, nil, err
	}
	if params.PublishComments && !descriptionSupportsPublishing(description) {
		return db.ExternalRootBinding{}, nil, ErrCommentPublishingUnavailable
	}
	root, err := instance.Client.ResolveRoot(ctx, connector.ResolveRootParams{Locator: params.Locator})
	if err != nil {
		return db.ExternalRootBinding{}, nil, err
	}
	if err := validateResolvedRoot(root); err != nil {
		return db.ExternalRootBinding{}, nil, err
	}
	if description.AccountIdentity != root.IdentityKey {
		return db.ExternalRootBinding{}, nil, fmt.Errorf("%w: description and root account differ", ErrConnectorIdentityChanged)
	}
	listedComments, err := instance.Client.ListComments(ctx, connector.ListCommentsParams{RootKey: root.Key})
	if err != nil {
		return db.ExternalRootBinding{}, nil, err
	}
	initialCommentRevisions := make([]db.ExternalCommentRevision, 0, len(listedComments.Comments))
	for _, comment := range listedComments.Comments {
		initialCommentRevisions = append(initialCommentRevisions, db.ExternalCommentRevision{
			ExternalID: comment.ID, Revision: comment.Revision,
		})
	}
	claimToken, err := katauid.New()
	if err != nil {
		return db.ExternalRootBinding{}, nil, fmt.Errorf("generate initial external root claim: %w", err)
	}
	claimStartedAt := time.Now().UTC()

	create := db.CreateExternalRootBindingParams{
		ProjectID: params.ProjectID, IssueID: params.IssueID,
		ConnectorInstance: params.ConnectorInstance, ExternalRootKey: root.Key,
		ExternalAccountKey: root.IdentityKey, Actor: params.Actor,
		ReceiveCommentsAfter: root.ObservedAt, PublishComments: params.PublishComments,
		UseLocalPublishFrontier:    params.PublishComments,
		UseCommentIdentityFrontier: true,
		InitialCommentRevisions:    initialCommentRevisions,
		InitialClaimToken:          claimToken,
		InitialClaimStartedAt:      claimStartedAt,
	}
	binding, bound, err := s.store.CreateExternalRootBinding(ctx, create)
	if err != nil {
		return db.ExternalRootBinding{}, nil, err
	}
	s.emitEvents(bound)
	events := []db.Event{bound}
	reconciled, err := s.immediateClaimedReconcile(ctx, binding.ID, binding.ClaimToken)
	events = append(events, reconciled...)
	if err != nil {
		return binding, events, fmt.Errorf("immediate reconcile external root: %w", err)
	}
	if _, err := s.store.IssueByID(ctx, params.IssueID); err != nil {
		return binding, events, fmt.Errorf("read reconciled issue: %w", err)
	}
	binding, err = s.store.ExternalRootBindingByID(ctx, binding.ID)
	if err != nil {
		return db.ExternalRootBinding{}, events, fmt.Errorf("read reconciled binding: %w", err)
	}
	return binding, events, nil
}

func descriptionSupportsPublishing(description connector.Description) bool {
	if strings.TrimSpace(description.SelfActorID) == "" {
		return false
	}
	return descriptionHasCapability(description, connector.CapabilityPublishComment)
}

func descriptionSupportsFields(description connector.Description) bool {
	return descriptionHasCapability(description, connector.CapabilityFields) &&
		descriptionHasCapability(description, connector.CapabilityConditionalFields)
}

func descriptionHasCapability(description connector.Description, want connector.Capability) bool {
	return slices.Contains(description.Capabilities, want)
}

// Pause disables reconciliation for an issue's active binding.
func (s *Service) Pause(ctx context.Context, issueID int64, actor, reason string) (db.ExternalRootBinding, db.Event, error) {
	binding, err := s.store.ExternalRootBindingByIssue(ctx, issueID)
	if err != nil {
		return db.ExternalRootBinding{}, db.Event{}, err
	}
	paused, event, err := s.store.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: binding.ID, Actor: actor, Reason: reason,
		StaleBefore: time.Now().UTC().Add(-s.claimStaleAfter),
	})
	if err == nil {
		s.emitEvents(event)
	}
	return paused, event, err
}

// Resume revalidates connector identity before enabling a paused binding.
func (s *Service) Resume(ctx context.Context, issueID int64, actor string) (db.ExternalRootBinding, db.Event, error) {
	binding, err := s.store.ExternalRootBindingByIssue(ctx, issueID)
	if err != nil {
		return db.ExternalRootBinding{}, db.Event{}, err
	}
	instance, err := s.requireInstance(binding.ConnectorInstance)
	if err != nil {
		return db.ExternalRootBinding{}, db.Event{}, err
	}
	description, err := s.describe(ctx, instance)
	if err != nil {
		return db.ExternalRootBinding{}, db.Event{}, err
	}
	if binding.PublishComments && !descriptionSupportsPublishing(description) {
		return db.ExternalRootBinding{}, db.Event{}, ErrCommentPublishingUnavailable
	}
	root, err := instance.Client.ReadRoot(ctx, connector.ReadRootParams{RootKey: binding.ExternalRootKey})
	if err != nil {
		return db.ExternalRootBinding{}, db.Event{}, err
	}
	if root.Key != binding.ExternalRootKey || root.IdentityKey != binding.ExternalAccountKey ||
		description.AccountIdentity != binding.ExternalAccountKey {
		return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: account or root does not match binding", ErrConnectorIdentityChanged)
	}
	resumed, event, err := s.store.ResumeExternalRootBinding(
		ctx, db.ExternalRootActionParams{BindingID: binding.ID, Actor: actor},
	)
	if err == nil {
		s.emitEvents(event)
	}
	return resumed, event, err
}

// Unbind permanently detaches an issue from its external root.
func (s *Service) Unbind(ctx context.Context, issueID int64, actor string) (db.ExternalRootBinding, db.Event, error) {
	binding, err := s.store.ExternalRootBindingByIssue(ctx, issueID)
	if err != nil {
		return db.ExternalRootBinding{}, db.Event{}, err
	}
	unbound, event, err := s.store.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: binding.ID, Actor: actor,
		StaleBefore: time.Now().UTC().Add(-s.claimStaleAfter),
	})
	if err == nil {
		s.emitEvents(event)
	}
	return unbound, event, err
}

// MapFieldParams selects one Kata planning field and one external field.
type MapFieldParams struct {
	ConnectorInstance string
	KataField         string
	ExternalField     string
}

// MapField validates and persists one connector-level field mapping.
func (s *Service) MapField(ctx context.Context, params MapFieldParams) (db.ExternalFieldMapping, error) {
	params.ConnectorInstance = strings.TrimSpace(params.ConnectorInstance)
	params.KataField = strings.TrimSpace(params.KataField)
	params.ExternalField = strings.TrimSpace(params.ExternalField)
	codec, ok := fieldCodecs[params.KataField]
	if !ok {
		return db.ExternalFieldMapping{}, fmt.Errorf("%w: unsupported Kata field", db.ErrExternalFieldMappingValidation)
	}
	instance, err := s.requireInstance(params.ConnectorInstance)
	if err != nil {
		return db.ExternalFieldMapping{}, err
	}
	description, err := s.describe(ctx, instance)
	if err != nil {
		return db.ExternalFieldMapping{}, err
	}
	if !descriptionSupportsFields(description) {
		return db.ExternalFieldMapping{}, ErrFieldSynchronizationUnavailable
	}
	listed, err := instance.Client.ListFields(ctx)
	if err != nil {
		return db.ExternalFieldMapping{}, err
	}
	descriptor, err := resolveExternalField(listed.Fields, params.ExternalField)
	if err != nil {
		return db.ExternalFieldMapping{}, err
	}
	if err := codec.ValidateExternalDescriptor(descriptor); err != nil {
		return db.ExternalFieldMapping{}, fmt.Errorf("%w: validate external field %q: %w", db.ErrExternalFieldMappingValidation, descriptor.ID, err)
	}
	return s.store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: params.ConnectorInstance, KataField: params.KataField,
		ExternalFieldID: descriptor.ID, ExternalFieldName: descriptor.DisplayName,
		AcceptedKinds: append([]string(nil), descriptor.AcceptedKinds...),
		Nullable:      descriptor.Nullable, Writable: descriptor.Writable,
		SchemaRevision: descriptor.SchemaRevision,
	})
}

// UnmapField removes one connector-level field mapping.
func (s *Service) UnmapField(ctx context.Context, connectorInstance, kataField string) (db.ExternalFieldMapping, error) {
	connectorInstance = strings.TrimSpace(connectorInstance)
	kataField = strings.TrimSpace(kataField)
	if _, ok := fieldCodecs[kataField]; !ok {
		return db.ExternalFieldMapping{}, fmt.Errorf("%w: unsupported Kata field", db.ErrExternalFieldMappingValidation)
	}
	if _, err := s.requireInstance(connectorInstance); err != nil {
		return db.ExternalFieldMapping{}, err
	}
	return s.store.UnmapExternalField(ctx, connectorInstance, kataField)
}

// ResolveFieldConflict applies one chosen candidate and clears the conflict.
func (s *Service) ResolveFieldConflict(
	ctx context.Context,
	issueID int64,
	kataField string,
	use string,
	actor string,
) (resolved db.ExternalFieldState, events []db.Event, returnErr error) {
	kataField = strings.TrimSpace(kataField)
	use = strings.TrimSpace(use)
	actor = strings.TrimSpace(actor)
	codec, ok := fieldCodecs[kataField]
	if issueID <= 0 || !ok || actor == "" {
		return resolved, events, fmt.Errorf("%w: issue, supported Kata field, and actor are required", db.ErrExternalRootValidation)
	}
	if use != "kata" && use != "external" {
		return resolved, events, fmt.Errorf("%w: field conflict resolution must use kata or external", db.ErrExternalRootValidation)
	}
	binding, err := s.store.ExternalRootBindingByIssue(ctx, issueID)
	if err != nil {
		return resolved, events, err
	}
	if !binding.Active || !binding.Enabled {
		return resolved, events, db.ErrExternalRootClaimLost
	}
	mappings, err := s.store.ListExternalFieldMappings(ctx, binding.ConnectorInstance)
	if err != nil {
		return resolved, events, err
	}
	var mapping db.ExternalFieldMapping
	for _, candidate := range mappings {
		if !candidate.Active || candidate.KataField != kataField {
			continue
		}
		if mapping.ID != 0 {
			return resolved, events, fmt.Errorf("%w: multiple active mappings for %s", db.ErrExternalRootValidation, kataField)
		}
		mapping = candidate
	}
	if mapping.ID == 0 {
		return resolved, events, fmt.Errorf("%w: %s", ErrExternalFieldNotFound, kataField)
	}
	states, err := s.store.ExternalFieldStates(ctx, binding.ID)
	if err != nil {
		return resolved, events, err
	}
	var conflicted db.ExternalFieldState
	for _, candidate := range states {
		if candidate.MappingID == mapping.ID {
			conflicted = candidate
			break
		}
	}
	if !conflicted.Conflicted {
		return resolved, events, db.ErrExternalFieldNotConflicted
	}

	claimToken, err := katauid.New()
	if err != nil {
		return resolved, events, fmt.Errorf("generate field conflict claim token: %w", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	claimed, acquired, err := s.store.ClaimExternalRootBindingForManualReconcile(
		ctx, binding.ID, claimToken, now, now.Add(-defaultExternalRootClaimStaleAfter),
	)
	if err != nil {
		return resolved, events, err
	}
	if !acquired {
		return resolved, events, db.ErrExternalRootClaimLost
	}
	parentCtx := ctx
	var lease *externalRootClaimLease
	defer func() {
		if lease != nil {
			if leaseErr := lease.stop(); leaseErr != nil {
				returnErr = errors.Join(returnErr, leaseErr)
			}
		}
		finalizeCtx, finalizeCancel := context.WithTimeout(
			context.WithoutCancel(parentCtx), externalRootFinalizationTimeout,
		)
		defer finalizeCancel()
		_, releaseErr := s.store.ReleaseExternalRootClaim(finalizeCtx, claimed.ID, claimToken)
		if releaseErr != nil && !errors.Is(releaseErr, db.ErrExternalRootClaimLost) {
			returnErr = errors.Join(returnErr, fmt.Errorf("release field conflict claim: %w", releaseErr))
		}
	}()
	lease, err = startExternalRootClaimLease(
		ctx, s.store, claimed.ID, claimToken, s.claimStaleAfter,
		func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) },
	)
	if err != nil {
		return resolved, events, err
	}
	ctx = lease.ctx
	claimedMappings, err := s.store.ListExternalFieldMappings(ctx, claimed.ConnectorInstance)
	if err != nil {
		return resolved, events, err
	}
	var claimedMapping db.ExternalFieldMapping
	for _, candidate := range claimedMappings {
		if !candidate.Active || candidate.KataField != kataField {
			continue
		}
		if candidate.ID != mapping.ID || claimedMapping.ID != 0 {
			return resolved, events, ErrFieldConflictChanged
		}
		claimedMapping = candidate
	}
	if claimedMapping.ID == 0 {
		return resolved, events, ErrFieldConflictChanged
	}
	mapping = claimedMapping
	claimedStates, err := s.store.ExternalFieldStates(ctx, claimed.ID)
	if err != nil {
		return resolved, events, err
	}
	conflicted = db.ExternalFieldState{}
	for _, candidate := range claimedStates {
		if candidate.MappingID == mapping.ID {
			conflicted = candidate
			break
		}
	}
	if !conflicted.Conflicted {
		return resolved, events, ErrFieldConflictChanged
	}

	instance, err := s.requireInstance(claimed.ConnectorInstance)
	if err != nil {
		return resolved, events, err
	}
	description, err := s.describe(ctx, instance)
	if err != nil {
		return resolved, events, err
	}
	if !descriptionSupportsFields(description) {
		return resolved, events, ErrFieldSynchronizationUnavailable
	}
	root, err := instance.Client.ReadRoot(ctx, connector.ReadRootParams{RootKey: claimed.ExternalRootKey})
	if err != nil {
		return resolved, events, err
	}
	if description.AccountIdentity != claimed.ExternalAccountKey || root.Key != claimed.ExternalRootKey ||
		root.IdentityKey != claimed.ExternalAccountKey {
		return resolved, events, fmt.Errorf("%w: account or root does not match binding", ErrConnectorIdentityChanged)
	}
	listed, err := instance.Client.ListFields(ctx)
	if err != nil {
		return resolved, events, err
	}
	if _, err := validateLiveFieldDescriptor(codec, mapping, listed.Fields); err != nil {
		return resolved, events, ErrFieldConflictChanged
	}
	issue, err := s.store.IssueByID(ctx, claimed.IssueID)
	if err != nil {
		return resolved, events, err
	}
	kataValue, err := readCanonicalKataField(codec, issue, mapping)
	if err != nil {
		return resolved, events, ErrFieldConflictChanged
	}
	snapshot := reconcileSnapshot{binding: claimed, client: instance.Client}
	externalValue, observedExternal, err := readMappedExternalFieldObserved(ctx, &snapshot, mapping)
	if err != nil {
		if isInvalidExternalFieldRead(err) {
			return resolved, events, ErrFieldConflictChanged
		}
		return resolved, events, err
	}
	storedKata, hasKata, err := decodeCanonicalStoredFieldValue(conflicted.ConflictKata, mapping)
	if err != nil || !hasKata {
		return resolved, events, ErrFieldConflictChanged
	}
	storedExternal, hasExternal, err := decodeCanonicalStoredFieldValue(conflicted.ConflictExternal, mapping)
	if err != nil || !hasExternal {
		return resolved, events, ErrFieldConflictChanged
	}
	selected := storedKata
	coordinatedTimezone := ""
	if use == "external" {
		selected = storedExternal
		coordinatedTimezone = coordinatedConflictTimezone(
			issue, mapping, claimedMappings, claimedStates, selected,
		)
	}
	kataCandidateStable := fieldValuesEqual(kataValue, storedKata)
	if use == "external" && !kataCandidateStable {
		kataCandidateStable = fieldValuesEqual(kataValue, selected) || coordinatedConflictKataDriftAllowed(
			issue, mapping, claimedMappings, claimedStates, storedKata, kataValue, storedExternal,
		)
	}
	externalCandidateStable := fieldValuesEqual(externalValue, storedExternal)
	if use == "kata" && !externalCandidateStable {
		externalCandidateStable = fieldValuesEqual(externalValue, selected)
	}
	if !kataCandidateStable || !externalCandidateStable {
		return resolved, events, ErrFieldConflictChanged
	}
	projected := issue
	if !fieldValuesEqual(kataValue, selected) {
		patch, patchErr := kataPatchWithCoordinatedTimezone(codec, issue, selected, coordinatedTimezone)
		if patchErr != nil {
			return resolved, events, patchErr
		}
		var event *db.Event
		projected, event, _, err = s.store.ApplyExternalFieldProjection(ctx, db.ExternalFieldProjectionParams{
			BindingID: claimed.ID, MappingID: mapping.ID, ClaimToken: claimToken,
			KataField: kataField, Patch: patch, ExpectedIssueRevision: issue.Revision,
			IntegrationActor: integrationActor(claimed),
		})
		if err != nil {
			return resolved, events, err
		}
		if event != nil {
			s.emitEvents(*event)
			events = append(events, *event)
		}
	}
	if use == "kata" && !fieldValuesEqual(externalValue, selected) {
		if err := renewExternalRootClaimBeforeCall(
			ctx, s.store, claimed.ID, claimToken, time.Now().UTC().Truncate(time.Millisecond),
		); err != nil {
			return resolved, events, err
		}
		_, err = instance.Client.WriteFields(ctx, connector.WriteFieldsParams{
			RootKey:  claimed.ExternalRootKey,
			Fields:   map[string]connector.FieldValue{mapping.ExternalFieldID: selected},
			Expected: map[string]connector.FieldValue{mapping.ExternalFieldID: observedExternal},
		})
		if err != nil {
			return resolved, events, err
		}
	}
	projected, err = s.store.IssueByID(ctx, projected.ID)
	if err != nil {
		return resolved, events, err
	}
	kataReadback, err := readCanonicalKataField(codec, projected, mapping)
	if err != nil {
		return resolved, events, ErrFieldConflictChanged
	}
	if !fieldValuesEqual(kataReadback, selected) {
		return resolved, events, ErrFieldConflictChanged
	}
	if use == "kata" {
		externalReadback, err := readMappedExternalField(ctx, &snapshot, mapping)
		if err != nil {
			if isInvalidExternalFieldRead(err) {
				return resolved, events, ErrFieldConflictChanged
			}
			return resolved, events, err
		}
		if !fieldValuesEqual(externalReadback, selected) {
			return resolved, events, ErrFieldConflictChanged
		}
	}
	baseline, err := marshalCanonicalFieldValue(selected)
	if err != nil {
		return resolved, events, err
	}
	resolved, resolvedEvent, err := s.store.ResolveExternalFieldConflict(ctx, db.ResolveExternalFieldConflictParams{
		BindingID: claimed.ID, MappingID: mapping.ID, ClaimToken: claimToken,
		Baseline: baseline, Actor: actor, At: time.Now().UTC(),
	})
	if err != nil {
		return resolved, events, err
	}
	s.emitEvents(resolvedEvent)
	events = append(events, resolvedEvent)
	return resolved, events, nil
}

func coordinatedConflictTimezone(
	issue db.Issue,
	mapping db.ExternalFieldMapping,
	mappings []db.ExternalFieldMapping,
	states []db.ExternalFieldState,
	selected connector.FieldValue,
) string {
	if selected.Kind != fieldKindLocalDateTime || selected.Timezone == "" {
		return ""
	}
	codec, ok := fieldCodecs[mapping.KataField]
	if !ok {
		return ""
	}
	current, err := readCanonicalKataField(codec, issue, mapping)
	if err != nil || current.Kind != fieldKindLocalDateTime || current.Timezone == selected.Timezone {
		return ""
	}
	stateByMapping := make(map[int64]db.ExternalFieldState, len(states))
	for _, state := range states {
		stateByMapping[state.MappingID] = state
	}
	foundSibling := false
	for _, sibling := range mappings {
		if !sibling.Active || sibling.ID == mapping.ID {
			continue
		}
		siblingCodec, ok := fieldCodecs[sibling.KataField]
		if !ok {
			return ""
		}
		siblingKata, err := readCanonicalKataField(siblingCodec, issue, sibling)
		if err != nil || siblingKata.Kind != fieldKindLocalDateTime || siblingKata.Timezone != current.Timezone {
			return ""
		}
		siblingState, ok := stateByMapping[sibling.ID]
		if !ok || !siblingState.Conflicted {
			return ""
		}
		siblingExternal, ok, err := decodeCanonicalStoredFieldValue(siblingState.ConflictExternal, sibling)
		if err != nil || !ok || siblingExternal.Kind != fieldKindLocalDateTime || siblingExternal.Timezone != selected.Timezone {
			return ""
		}
		foundSibling = true
	}
	if !foundSibling {
		return ""
	}
	return selected.Timezone
}

func coordinatedConflictKataDriftAllowed(
	issue db.Issue,
	mapping db.ExternalFieldMapping,
	mappings []db.ExternalFieldMapping,
	states []db.ExternalFieldState,
	storedKata connector.FieldValue,
	currentKata connector.FieldValue,
	selectedExternal connector.FieldValue,
) bool {
	if storedKata.Kind != fieldKindLocalDateTime || currentKata.Kind != fieldKindLocalDateTime ||
		selectedExternal.Kind != fieldKindLocalDateTime || storedKata.Value != currentKata.Value ||
		storedKata.Timezone == currentKata.Timezone || currentKata.Timezone != selectedExternal.Timezone {
		return false
	}
	stateByMapping := make(map[int64]db.ExternalFieldState, len(states))
	for _, state := range states {
		stateByMapping[state.MappingID] = state
	}
	foundSibling := false
	for _, sibling := range mappings {
		if !sibling.Active || sibling.ID == mapping.ID {
			continue
		}
		siblingCodec, ok := fieldCodecs[sibling.KataField]
		if !ok {
			return false
		}
		siblingKata, err := readCanonicalKataField(siblingCodec, issue, sibling)
		if err != nil || siblingKata.Kind != fieldKindLocalDateTime || siblingKata.Timezone != selectedExternal.Timezone {
			return false
		}
		siblingState, ok := stateByMapping[sibling.ID]
		if !ok || siblingState.Conflicted {
			return false
		}
		siblingBaseline, ok, err := decodeCanonicalStoredFieldValue(siblingState.Baseline, sibling)
		if err != nil || !ok || siblingBaseline.Kind != fieldKindLocalDateTime ||
			siblingBaseline.Timezone != selectedExternal.Timezone || !fieldValuesEqual(siblingBaseline, siblingKata) {
			return false
		}
		foundSibling = true
	}
	return foundSibling
}

func (s *Service) requireInstance(id string) (Instance, error) {
	if s.registry == nil {
		return Instance{}, fmt.Errorf("%w: %s", ErrConnectorInstanceNotFound, strings.TrimSpace(id))
	}
	instance, ok := s.registry.instance(id)
	if !ok {
		return Instance{}, fmt.Errorf("%w: %s", ErrConnectorInstanceNotFound, strings.TrimSpace(id))
	}
	return instance, nil
}

func (s *Service) describe(ctx context.Context, instance Instance) (connector.Description, error) {
	description, err := instance.Client.Describe(ctx)
	if err != nil {
		if !parentContextEnded(ctx, err) {
			s.registry.recordDescribeHealthError(instance.Config.ID, err)
		}
		return connector.Description{}, err
	}
	if err := s.registry.acceptDescription(instance.Config.ID, description); err != nil {
		return connector.Description{}, err
	}
	return description, nil
}

func validateResolvedRoot(root connector.Root) error {
	if strings.TrimSpace(root.Key) == "" || strings.TrimSpace(root.Key) != root.Key ||
		strings.TrimSpace(root.IdentityKey) == "" || strings.TrimSpace(root.IdentityKey) != root.IdentityKey {
		return fmt.Errorf("%w: root and account identities must be nonempty and canonical", db.ErrExternalRootValidation)
	}
	if root.ObservedAt.IsZero() {
		return fmt.Errorf("%w: root observation time is required", db.ErrExternalRootValidation)
	}
	return nil
}

func resolveExternalField(fields []connector.FieldDescriptor, selector string) (connector.FieldDescriptor, error) {
	selector = strings.TrimSpace(selector)
	byID := make([]connector.FieldDescriptor, 0, 1)
	for _, field := range fields {
		if field.ID == selector {
			byID = append(byID, field)
		}
	}
	if len(byID) == 1 {
		return byID[0], nil
	}
	if len(byID) > 1 {
		return connector.FieldDescriptor{}, fmt.Errorf("%w: stable ID %q", ErrExternalFieldAmbiguous, selector)
	}
	byName := make([]connector.FieldDescriptor, 0, 1)
	for _, field := range fields {
		if field.DisplayName == selector {
			byName = append(byName, field)
		}
	}
	switch len(byName) {
	case 0:
		return connector.FieldDescriptor{}, fmt.Errorf("%w: %s", ErrExternalFieldNotFound, selector)
	case 1:
		return byName[0], nil
	default:
		return connector.FieldDescriptor{}, fmt.Errorf("%w: display name %q", ErrExternalFieldAmbiguous, selector)
	}
}
