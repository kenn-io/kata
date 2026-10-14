package db

import (
	"context"
	"time"
)

// RelayResetBackend is the row access one store gives the shared relay reset
// protocol inside a single transaction. Each method is one query or statement
// in that backend's SQL plus whatever row locking the backend needs. Every
// protocol decision lives in RelayResetProtocol, so the SQLite and PostgreSQL
// stores cannot drift on what a reset replaces and keeps, which deliveries a
// forward supersedes, which downstream work blocks an install, or how a
// checkpoint is translated per hop.
type RelayResetBackend interface {
	// ProjectUID returns the UID of a project row.
	ProjectUID(ctx context.Context, projectID int64) (string, error)
	// RelayBinding loads the project's federation binding, or ErrNotFound.
	// PostgreSQL locks the row for the rest of the transaction.
	RelayBinding(ctx context.Context, projectID int64) (FederationBinding, error)
	// RelayUpstreamDeliveriesOwed reports an unacknowledged outbox row for the
	// project's upstream hop.
	RelayUpstreamDeliveriesOwed(ctx context.Context, projectUID, bindingUID string) (bool, error)
	// RelayLocalEventsAwaitingReceipt lists this instance's own events in the
	// upstream hop's current epoch that have no root receipt yet, in outbox
	// order.
	RelayLocalEventsAwaitingReceipt(ctx context.Context, projectUID, bindingUID string, epoch int64, originUID string) ([]RelayEventRef, error)
	// PendingClaimRequestsOpen reports an unresolved, unrejected claim request.
	PendingClaimRequestsOpen(ctx context.Context, projectID int64) (bool, error)
	// RelayDownstreamDeliveriesOwed reports an unacknowledged outbox row owed
	// to any live downstream enrollment of the project.
	RelayDownstreamDeliveriesOwed(ctx context.Context, projectUID string) (bool, error)
	// RelayDownstreamEnrollmentsActive reports an unrevoked relay enrollment.
	RelayDownstreamEnrollmentsActive(ctx context.Context, projectID int64) (bool, error)
	// DeleteRelayBindingState removes one binding's outbox, inbox and cursors.
	DeleteRelayBindingState(ctx context.Context, bindingUID string) error
	// LockProject verifies the project row exists, or ErrNotFound. PostgreSQL
	// locks the row for the rest of the transaction.
	LockProject(ctx context.Context, projectID int64) error
	// RejectIssueSyncedProject returns ErrIssueSyncFederationBinding when the
	// project has an enabled issue sync binding.
	RejectIssueSyncedProject(ctx context.Context, projectID int64) error
	// RejectExternalRootProject returns ErrExternalRootFederationConflict when
	// the project has an active external root binding.
	RejectExternalRootProject(ctx context.Context, projectID int64) error
	// DisableRelayPush enables the spoke binding and disables its pushes.
	DisableRelayPush(ctx context.Context, projectID int64) error
	// ReconcileBindingTransitionLinks applies the binding transition's link
	// reconciliation.
	ReconcileBindingTransitionLinks(ctx context.Context, previous *FederationBinding, next FederationBinding) error

	// Meta returns one daemon-local metadata value and whether the key exists.
	Meta(ctx context.Context, key string) (string, bool, error)
	// PutMeta inserts or replaces one daemon-local metadata value.
	PutMeta(ctx context.Context, key, value string) error
	// DeleteMeta removes one daemon-local metadata key.
	DeleteMeta(ctx context.Context, key string) error
	// RootPin loads the project's root key pin for keyID, or the active pin
	// when keyID is empty; ErrNotFound when the project has no such pin.
	RootPin(ctx context.Context, projectUID, keyID string) (RootKeyPin, error)
	// RelayUnacknowledgedEnvelopes returns the raw envelopes of unacknowledged
	// outbox rows for the binding's epoch, in row order: every row of
	// keepStream plus the rows of other streams above boundaryID.
	RelayUnacknowledgedEnvelopes(ctx context.Context, bindingUID string, epoch int64, keepStream string, boundaryID int64) ([]string, error)
	// SetRelayGrantEpoch moves one enrollment to the given relay epoch.
	SetRelayGrantEpoch(ctx context.Context, grantID, epoch int64) error
	// QueueRelayBody queues one delivery for the grant through the ordinary
	// outbox path, honoring the relay forward path and artifact source UID
	// carried by ctx.
	QueueRelayBody(ctx context.Context, body RelayQueuedBody) error

	// RelayPostCheckpointReceipts lists the project's stored root receipts
	// above the (epoch, sequence) position, each joined to its event row,
	// ordered by position and then event UID.
	RelayPostCheckpointReceipts(ctx context.Context, projectID int64, projectUID string, epoch, sequence int64) ([]RelayRetainedReceiptRow, error)
	// StoredEvent loads one event row by ID.
	StoredEvent(ctx context.Context, eventID int64) (Event, error)

	// RelayServingBinding loads the binding under which the project serves
	// current-protocol downstream relays, or ErrNotFound.
	RelayServingBinding(ctx context.Context, projectID int64) (FederationBinding, error)
	// UpstreamRelayGrant loads the live grant of the project's upstream hop.
	UpstreamRelayGrant(ctx context.Context, bindingUID string) (FederationEnrollment, error)
	// RelayGrant loads a live downstream grant that holds capability, under
	// the enrollment's transaction fence.
	RelayGrant(ctx context.Context, bindingUID, capability string) (FederationEnrollment, error)
	// SeedRelayArtifactManifests queues the project's artifact manifests for
	// the grant's current epoch.
	SeedRelayArtifactManifests(ctx context.Context, grant FederationEnrollment, projectUID string) error
	// RelayAcceptedInboxPage lists accepted inbox rows of the page's stream
	// and epoch with a hop sequence above After, in sequence order.
	RelayAcceptedInboxPage(ctx context.Context, page RelayRetainedPage) ([]RelayRetainedEnvelopeRow, error)
	// RelayOutboxPage lists outbox rows of the page's stream and epoch with a
	// row ID above After, in ID order; Sequence carries the row ID.
	RelayOutboxPage(ctx context.Context, page RelayRetainedPage) ([]RelayRetainedEnvelopeRow, error)

	// RelayActiveDownstreamGrants lists the project's unrevoked
	// current-protocol relay enrollments in ID order.
	RelayActiveDownstreamGrants(ctx context.Context, projectID int64) ([]FederationEnrollment, error)
	// RelayUnacknowledgedDeliveries lists the binding's unacknowledged outbox
	// rows for epoch in ID order, each with whether a root receipt exists for
	// its source.
	RelayUnacknowledgedDeliveries(ctx context.Context, bindingUID string, epoch int64) ([]RelayDeliveryRow, error)
	// RelayOutboxEpochSummary reports the binding's greatest outbox row ID for
	// epoch and its unacknowledged event and artifact rows.
	RelayOutboxEpochSummary(ctx context.Context, bindingUID string, epoch int64) (RelayOutboxEpochSummary, error)
	// DeleteUnemittedRelayOutbox removes the binding's never-emitted outbox
	// rows for epoch.
	DeleteUnemittedRelayOutbox(ctx context.Context, bindingUID string, epoch int64) error

	// RelayQuarantineOpen reports a quarantined delivery not yet skipped.
	RelayQuarantineOpen(ctx context.Context, projectID int64) (bool, error)
	// ExternalRootBindingExists reports any external root binding row.
	ExternalRootBindingExists(ctx context.Context, projectID int64) (bool, error)
	// ValidateRelayResetEvents runs the backend's federation event validation
	// over the decoded checkpoint events as a complete project replacement.
	ValidateRelayResetEvents(projectUID string, events []RemoteEvent) error
	// ReplaceRelayHistory deletes every project event except daemon-local
	// catalog history (RelayResetRetainedEventTypesSQL) and clears the
	// federated projection. PostgreSQL serializes the event sequence first.
	ReplaceRelayHistory(ctx context.Context, projectID int64, projectUID string) error
	// DeleteProjectProvenance removes the project's event and entity
	// provenance rows.
	DeleteProjectProvenance(ctx context.Context, projectUID string) error
	// InsertRetiredRootKey stores a historical root key as inactive.
	InsertRetiredRootKey(ctx context.Context, key RootKeyPin) error
	// InsertRelayResetEvent inserts one checkpoint event with its original
	// identity, hash and clock.
	InsertRelayResetEvent(ctx context.Context, projectID int64, event RemoteEvent) error
	// MaterializeFederatedProject rebuilds the projection from the project's
	// events, reconciling links.
	MaterializeFederatedProject(ctx context.Context, projectID int64) error
	// DeleteOrphanRelayArtifacts removes embedding artifacts whose issue no
	// longer exists in the project.
	DeleteOrphanRelayArtifacts(ctx context.Context, projectUID string) error
	// DeleteOrphanPendingCreations removes pending creations whose entity no
	// longer exists in the project.
	DeleteOrphanPendingCreations(ctx context.Context, projectID int64, projectUID string) error
	// RetainedRelayArtifact loads the project's artifact with digest when it
	// exists for a current issue and its staging has not expired at now.
	RetainedRelayArtifact(ctx context.Context, projectUID, digest string, now time.Time) (RelayRetainedArtifactRow, bool, error)
	// ActivateStagedRelayArtifact clears the artifact's staging expiry.
	ActivateStagedRelayArtifact(ctx context.Context, projectUID, digest string) error
	// InsertEventProvenance stores one root receipt with its indexed columns.
	InsertEventProvenance(ctx context.Context, projectUID string, receipt AttributionReceipt, raw string) error
	// InsertEntityProvenance stores one creation reference.
	InsertEntityProvenance(ctx context.Context, projectUID string, ref EntityProvenance) error
	// DeletePendingCreation removes the pending creation a reference settles.
	DeletePendingCreation(ctx context.Context, projectUID, kind, entityUID string) error
	// UpdateRelayBindingConfig replaces the project binding's relay config.
	UpdateRelayBindingConfig(ctx context.Context, projectID int64, raw string) error
	// InsertRelayCursor starts one hop stream cursor at its baseline.
	InsertRelayCursor(ctx context.Context, seed RelayCursorSeed) error
	// ReserveAttributionUIReset marks the project's attribution UI state as
	// reset.
	ReserveAttributionUIReset(ctx context.Context, projectUID string) error

	// Project loads one project row.
	Project(ctx context.Context, projectID int64) (Project, error)
	// RelayCheckpointNeedsRefresh reports whether a later root purge or an
	// installed upstream checkpoint made the retained checkpoint obsolete.
	RelayCheckpointNeedsRefresh(ctx context.Context, projectUID string, checkpoint RelayResetCheckpoint) (bool, error)
	// LatestEventID returns the project's greatest event row ID, or zero.
	LatestEventID(ctx context.Context, projectID int64) (int64, error)
	// InsertFederationBaselineEvents writes the project's baseline records
	// and returns the boundary event.
	InsertFederationBaselineEvents(ctx context.Context, project Project, actor string) (Event, error)
	// InsertRelayCheckpointStateEvent writes one current-state record at the
	// given clock.
	InsertRelayCheckpointStateEvent(ctx context.Context, state RelayCheckpointStateEvent) error
	// RelayCheckpointRecords loads the records a checkpoint serializes:
	// history below boundaryID and current-state records at or above it.
	RelayCheckpointRecords(ctx context.Context, project Project, boundaryID int64) (RelayCheckpointRecords, error)
	// RelayCheckpointCapacity is the configured checkpoint byte capacity.
	RelayCheckpointCapacity() int64
	// MetaValuesWithPrefix lists daemon-local metadata values whose key
	// starts with prefix.
	MetaValuesWithPrefix(ctx context.Context, prefix string) ([]string, error)
	// RelayOutboxMaxID returns the project's greatest outbox row ID for the
	// stream across every binding, or zero.
	RelayOutboxMaxID(ctx context.Context, projectUID, stream string) (int64, error)
}

// RelayEventRef identifies one outbox event delivery by its source identity.
type RelayEventRef struct {
	EventUID    string
	ContentHash string
}

// RelayQueuedBody is one delivery the protocol queues for a downstream grant.
type RelayQueuedBody struct {
	ProjectID  int64
	ProjectUID string
	Stream     string
	SourceUID  string
	SourceHash string
	Body       []byte
	Grant      FederationEnrollment
}

// RelayResetProtocol runs the relay reset decisions over one backend
// transaction. InstanceUID is the daemon running the protocol.
type RelayResetProtocol struct {
	Backend     RelayResetBackend
	InstanceUID string
}
