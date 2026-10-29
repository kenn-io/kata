// Package dbtest provides backend-neutral behavioral checks for db.Storage
// implementations. Backend packages supply only a fresh-store factory and an
// explicit list of temporary parity gaps.
package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Backend configures one conformance run. Open must return a fresh store for
// every subtest. ExpectedFailures is an exact xfail manifest: a listed
// scenario must fail with the configured error, and unexpectedly passing it
// fails the suite until the stale entry is removed.
type Backend struct {
	Name                                string
	Open                                func(t *testing.T) db.Storage
	OpenSame                            func(t *testing.T, store db.Storage) db.Storage
	InstallExternalRootClock            func(db.Storage, func() time.Time) func()
	InstallRelayHistoryFullScanObserver func(db.Storage, func()) func()
	OpenWithRelayCheckpointCapacity     func(t *testing.T, capacity int64) db.Storage
	SeedLegacyPendingClaim              func(context.Context, db.Storage, string) error
	SeedClaimViolation                  func(context.Context, db.Storage, db.Project, db.Issue, string, jsontext.Value) error
	SeedUnsupportedFederationEvent      func(context.Context, db.Storage, db.Project, string) error
	// DetachEventIssueID clears an event's local issue row ID so it names its
	// subject by UID alone, as a relayed or detached-replica event does.
	DetachEventIssueID             func(context.Context, db.Storage, int64) error
	BackdateCommentCreated         func(context.Context, db.Storage, int64, time.Time) error
	InstallEnrollmentInsertFailure func(context.Context, db.Storage) (func() error, error)
	InstallEnrollmentRotationStage func(db.Storage, func(context.Context) error) func()
	SetArtifactStagingExpiry       func(ctx context.Context, store db.Storage, projectUID, digest, expiry string) error
	ArtifactStagingExpiry          func(ctx context.Context, store db.Storage, projectUID, digest string) (*string, error)
	// InstallRelayAckRowCounter counts every federation_relay_outbox row that an
	// acknowledgement UPDATE matches from installation until remove runs.
	InstallRelayAckRowCounter func(ctx context.Context, store db.Storage) (count func(context.Context) (int64, error), remove func() error, err error)
	ExpectedFailures          map[string]error
}

type scenario struct {
	name           string
	methods        []string
	run            func(t *testing.T, store db.Storage) error
	runWithBackend func(t *testing.T, store db.Storage, backend Backend) error
}

var storageScenarios = []scenario{
	{name: "setup finished delivery", methods: []string{"PrepareSetupFinished", "CompleteSetupFinished", "SystemProject", "CloseIssue", "SoftDeleteIssue", "RemoveProject"}, runWithBackend: checkSetupFinishedDelivery},
	{name: "comment graph project access", methods: []string{"CreateProject", "CreateIssue", "CreateComment", "MoveIssueProject", "ReadCommentGraph"}, run: checkCommentGraphProjectAccess},
	{name: "recurring adoption snapshots", run: checkRecurringAdoptionSnapshots},
	{name: "recurring tombstone adoption", run: checkRecurringTombstoneAdoption},
	{name: "recurring adoption refusal", run: checkRecurringAdoptionRefusal},
	{name: "recurring moved child adoption refusal", methods: []string{"CreateProject", "CreateIssue", "PutCronJob", "DispatchRecurringCron", "IssueByUID", "MoveIssueProject", "AdoptProjectIntoFederation", "EnableProjectFederation", "ReadFederation", "MaxEventID", "ProjectByUID"}, run: checkRecurringMovedChildAdoptionRefusal},
	{name: "recurring template protection", run: checkRecurringTemplateProtection},
	{name: "recurring feature probe candidates", run: checkRecurringFeatureProbeCandidates},
	{name: "recurring recovery", runWithBackend: checkRecurringRecovery},
	{name: "recurring parent project move", methods: []string{"CreateProject", "CreateIssue", "PutCronJob", "IssueByID", "IssueByUID", "CronJob", "MaxEventID", "MoveIssueProject"}, run: checkRecurringParentProjectMove},
	{name: "recurring metadata attachment", methods: []string{"CreateProject", "CreateIssue", "PutCronJob", "IssueByID", "IssueByUID", "DispatchRecurringCron", "CreateRecurrenceForIssue"}, runWithBackend: checkRecurringMetadataAttachment},
	{name: "recurring owner race and fence", runWithBackend: checkRecurringOwnerRaceAndFence},
	{name: "recurring spoke authority", run: checkRecurringSpokeAuthority},
	{name: "recurring federation", runWithBackend: checkRecurringFederation},
	{name: "recurring atomic rollback", runWithBackend: checkRecurringAtomicRollback},
	{name: "recurring lifecycle", run: checkRecurringLifecycle},
	{name: "recurring metadata protection", run: checkRecurringMetadataProtection},
	{name: "recurring project metadata keys", run: checkRecurringProjectMetadataKeys},
	{name: "recurring cron", methods: []string{"ListRecurringCronJobs", "DispatchRecurringCron"}, runWithBackend: checkRecurringCron},
	{name: "close transcript", run: checkCloseTranscript},
	{name: "comment notifications", methods: []string{"CreateComment", "PatchIssueMetadata"}, run: checkCommentNotifications},
	{name: "comment metadata multi issue", methods: []string{"CreateComment", "CreateIssue", "CreateProject", "EventsAfter", "IssueByID", "MaxEventID", "PatchIssueMetadata"}, run: checkCommentMetadataMultiIssue},
	{name: "metadata patch transaction hook", methods: []string{"CreateIssue", "CreateProject", "EventsAfter", "PatchIssueMetadata"}, run: checkMetadataPatchTransactionHook},
	{name: "comment graph deleted source", methods: []string{"CreateComment", "CreateIssue", "CreateProject", "IssueByID", "MoveIssueProject", "ReadCommentGraph", "SoftDeleteIssue"}, run: checkCommentGraphDeletedSource},
	{name: "selected comment graph", methods: []string{"CreateProject", "CreateIssue", "CreateComment", "EditComment", "ImportReplay", "ReadCommentGraph", "SoftDeleteIssue"}, run: checkSelectedCommentGraph},
	{name: "comment graph purge evidence order", methods: []string{"CreateComment", "CreateIssue", "CreateProject", "ImportReplay", "IssueByID", "IssueByUID", "MoveIssueProject", "ProjectByUID", "PurgeIssue", "ReadCommentGraph"}, run: checkCommentGraphPurgeEvidenceOrder},
	{name: "comment graph purge evidence from hidden source", methods: []string{"CreateComment", "ImportReplay", "IssueByUID", "PurgeIssue", "ReadCommentGraph", "SoftDeleteIssue"}, run: checkCommentGraphPurgeEvidenceFromHiddenSource},
	{name: "comment event export after target deletion", methods: []string{"CreateComment", "CreateIssue", "CreateProject", "EventsByUIDs", "ExportComments", "ExportEvents", "ExportExternalFieldMappings", "ExportExternalFieldStates", "ExportExternalRootBindings", "ExportFederationBindings", "ExportFederationEnrollments", "ExportFederationQuarantine", "ExportFederationSyncStatus", "ExportImportMappings", "ExportIssueClaims", "ExportIssueLabels", "ExportIssueSyncBindings", "ExportIssueSyncStatus", "ExportIssues", "ExportLinks", "ExportMeta", "ExportPendingClaimRequests", "ExportProjectAliases", "ExportProjectPurgeLog", "ExportProjects", "ExportPurgeLog", "ExportRecurrences", "ExportSequences", "ImportReplay", "ProjectByUID", "PurgeIssue", "ReadCommentGraph", "SoftDeleteIssue"}, runWithBackend: checkCommentEventExportAfterTargetDeletion},
	{name: "move returns committed event", methods: []string{"CreateIssue", "CreateProject", "EventsAfter", "MaxEventID", "MoveIssueProject"}, run: checkMoveReturnsCommittedEvent},
	{name: "moved issue ordinary purge resets", methods: []string{"MoveIssueProject", "PurgeIssue", "PurgeResetCheck"}, run: checkMovedIssueOrdinaryPurgeResets},
	{name: "moved issue adoption resets", methods: []string{"CreateProject", "CreateIssue", "MaxEventID", "MoveIssueProject", "EventsAfter", "AdoptProjectIntoFederation", "PurgeResetCheck"}, run: checkMovedIssueAdoptionResets},
	{name: "comment reply project purge lineage", methods: []string{"CreateComment", "CreateIssue", "CreateProject", "EventsByUIDs", "ExportPurgeLog", "MoveIssueProject", "PurgeProject", "ReadCommentGraph", "RemoveProject"}, run: checkCommentReplyProjectPurgeLineage},
	{name: "comment reply purge", methods: []string{"PurgeIssue", "CommentsByIssue"}, run: checkCommentReplyPurge},
	{name: "reply-only purge advances snapshot cursor", methods: []string{"CreateComment", "CreateIssue", "ImportReplay", "IssueByUID", "ProjectByUID", "PurgeIssue"}, run: checkReplyOnlyPurgeAdvancesSnapshotCursor},
	{name: "imported comment purge advances snapshot cursor", methods: []string{"ImportReplay", "IssueByUID", "ProjectByUID", "PurgeIssue"}, run: checkImportedCommentPurgeAdvancesSnapshotCursor},
	{name: "eventless archived project purge advances snapshot cursor", methods: []string{"ImportReplay", "ProjectByUID", "PurgeProject"}, run: checkEventlessArchivedProjectPurgeAdvancesSnapshotCursor},
	{name: "comment snapshot marks purged reply target removed", methods: []string{"CreateComment", "CreateIssue", "CreateProject", "PurgeIssue", "ReadCommentGraph"}, run: checkCommentSnapshotPurgedReplyTarget},
	{name: "comment reply replay", methods: []string{"ExportComments", "ImportReplay"}, runWithBackend: checkCommentReplyReplay},
	{name: "comment reply snapshot", methods: []string{"EnableProjectFederation", "EventsAfter"}, run: checkCommentReplySnapshot},
	{name: "comment reply federation", methods: []string{"InsertRemoteEvent", "MaterializeFederatedProject"}, run: checkCommentReplyFederation},
	{name: "comment reply envelope", methods: []string{"CreateComment"}, run: checkCommentReplyEnvelope},
	{name: "comment issue ids by uids", methods: []string{"CommentIssueIDsByUIDs"}, run: checkCommentIssueIDsByUIDs},
	{name: "issues by uids", methods: []string{"CreateProject", "CreateIssue", "SoftDeleteIssue", "IssueByUID", "IssuesByUIDs"}, run: checkIssuesByUIDs},
	{name: "comment replies", methods: []string{"CreateProject", "CreateIssue", "CreateComment", "EditComment", "CommentsByIssue"}, run: checkCommentReplies},
	{name: "creation pagination precision", methods: []string{"ListIssues", "ListAllIssues", "SearchFTS"}, run: checkCreationPaginationPrecision},
	{name: "list pagination", methods: []string{"ListIssues", "ListAllIssues", "CountIssues", "CountAllIssues"}, run: checkListPagination},
	{name: "search pagination", methods: []string{"SearchFTS", "SearchFTSAny"}, run: checkSearchPagination},
	{name: "relay ingress", methods: []string{"AcceptRelayDeliveries", "CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "IssueByUID", "LinksByIssue", "MaterializeFederatedProject", "PinRootAuthority", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error { RunRelayIngressAtomicity(t, store); return nil }},
	{name: "relay topology", methods: []string{"SetRelayBindingConfig"}, run: func(t *testing.T, store db.Storage) error { RunRelayTopology(t, store); return nil }},
	{name: "relay enrollment", methods: []string{"CreateRelayEnrollment"}, run: func(t *testing.T, store db.Storage) error { RunRelayEnrollmentScope(t, store); return nil }},
	{name: "relay cross-project boundary", methods: []string{"AdoptProjectIntoFederation", "AuthorizeFederationToken", "CheckRelayProjectLinks", "CloseIssueWithEvents", "CreateAPIToken", "CreateIssue", "CreateLink", "CreateLinkAndEvent", "CreateProject", "CreateRelayEnrollment", "DeleteLinkAndEvent", "EditIssueAtomic", "EventsAfter", "FederationBindingByProject", "InstanceUID", "IssueByID", "LinkByID", "LinksByIssue", "MoveIssueProject", "ParentOf", "PendingRelayDeliveries", "PinRootAuthority", "ProjectByID", "SetRelayBindingConfig", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error { RunRelayCrossProjectBoundary(t, store); return nil }},
	{name: "relay checkpoint capacity", methods: []string{"CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "ListFederationEnrollments", "PinRootAuthority", "UpsertFederationBinding"}, runWithBackend: func(t *testing.T, _ db.Storage, backend Backend) error {
		RunRelayCheckpointCapacity(t, backend)
		return nil
	}},
	{name: "relay checkpoint status bounds encoded actor", methods: []string{"AckRelayDeliveries", "CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "PendingRelayDeliveries", "PinRootAuthority", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayCheckpointStatusBoundsEncodedActor(t, store)
		return nil
	}},
	{name: "relay checkpoint admission matches reset", methods: []string{"AckRelayDeliveries", "CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "PendingRelayDeliveries", "PinRootAuthority", "UpsertFederationBinding"}, runWithBackend: func(t *testing.T, _ db.Storage, backend Backend) error {
		RunRelayCheckpointAdmissionMatchesReset(t, backend)
		return nil
	}},
	{name: "relay enrollment history validation cache", methods: []string{"CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "CreateComment", "ProjectAccessTransactionFence", "RemoveProject", "PurgeProject", "PinRootAuthority", "RevokeFederationEnrollment", "UpsertFederationBinding"}, runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
		RunRelayEnrollmentHistoryValidationCache(t, store, backend)
		return nil
	}},
	{name: "relay ingest returns local issue ids", methods: []string{"AcceptRelayDeliveries", "CreateAPIToken", "CreateProject", "CreateRelayEnrollment", "IssueByUID", "PinRootAuthority", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayIngestReturnsLocalIssueIDs(t, store)
		return nil
	}},
	{name: "relay delivery", methods: []string{"PendingRelayDeliveries", "AckRelayDeliveries", "ExportRelayState"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayOutboxAtomicity(t, store)
		seen := 0
		for record, err := range store.ExportRelayState(t.Context()) {
			require.NoError(t, err)
			require.NoError(t, db.ValidateRelayDeliveryRecord(record))
			seen++
		}
		require.Positive(t, seen)
		return nil
	}},
	{name: "relay link boundary keeps same-project effects", methods: []string{"AcceptRelayDeliveries", "CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "DeleteLinkAndEvent", "IngestFederationEvents", "IssueByUID", "LinksByIssue", "MaterializeFederatedProject", "PinRootAuthority", "RevokeFederationEnrollment", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayLinkBoundaryKeepsSameProjectEffects(t, store)
		return nil
	}},
	{name: "relay link boundary follows active grants", methods: []string{"CreateAPIToken", "CreateIssue", "CreateLinkAndEvent", "CreateProject", "CreateRelayEnrollment", "IngestFederationEvents", "LinksByIssue", "MaterializeFederatedProject", "PinRootAuthority", "RevokeFederationEnrollment", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayLinkBoundaryFollowsActiveGrants(t, store)
		return nil
	}},
	{name: "relay deferred link guard outlives binding", methods: []string{"AcceptRelayDeliveries", "CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "LinksByIssue", "MaterializeFederatedProject", "PinRootAuthority", "RevokeFederationEnrollment", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayDeferredLinkGuardOutlivesBinding(t, store)
		return nil
	}},
	{name: "relay acknowledgement touches only unacknowledged rows", methods: []string{"PendingRelayDeliveries", "AckRelayDeliveries", "ExportRelayState"}, runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
		RunRelayAckTouchesOnlyUnacknowledgedRows(t, store, backend)
		return nil
	}},

	{name: "root key rotation", methods: []string{"RotateRootAuthority", "RootKeyTransitions"}, run: func(t *testing.T, store db.Storage) error {
		RunRootKeyRotation(t, store)
		return nil
	}},

	{name: "root attribution", methods: []string{"PinRootAuthority", "RootAuthority", "RecordRootAttribution", "ApplyUpstreamAttribution", "EntityAttribution", "AttributionReceiptsAfter", "ExportAttribution"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayAttribution(t, store)
		RunUpstreamAttribution(t, store)
		return nil
	}},
	{name: "project access", methods: []string{"CreateTeam", "TeamByUID", "ListTeams", "DeleteTeam", "TeamMembers", "SetTeamMembership", "MigrateTeamActor", "ProjectAccessPolicy", "SetProjectAccessPolicy", "AccessibleProjectUIDs", "AnonymousAccessibleProjectUIDs", "ProjectAccessRevision", "ProjectAccessTransactionFence", "ExportProjectAccess"}, run: func(t *testing.T, store db.Storage) error {
		RunProjectAccessMergeIsolation(t, store)
		RunProjectAccessConformance(t, store)
		RunProjectAccessTokenEnrollment(t, store)
		return nil
	}},
	{name: "archived project portable export replay", methods: []string{"CreateProject", "CreateIssue", "RemoveProject", "UpsertFederationBinding", "PinRootAuthority", "ExportProjects", "ExportIssues", "ExportEvents", "ExportAttribution", "ImportReplay"}, runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
		RunArchivedPortableDataExport(t, store, backend)
		return nil
	}},
	{name: "parent replacement with removed expected link", methods: []string{"CreateProject", "CreateIssue", "CreateLinkAndEvent", "DeleteLinkAndEvent", "ParentOf"}, run: func(t *testing.T, store db.Storage) error {
		RunParentReplacementMissingExpectedLink(t, store)
		return nil
	}},
	{name: "local comment replies", methods: []string{"CreateProject", "CreateIssue", "CreateComment", "CloseIssue", "SoftDeleteIssue", "CommentsByIssue"}, run: checkLocalCommentReplies},
	{name: "concurrent local comment replies", methods: []string{"CreateProject", "CreateIssue", "CreateComment", "CommentsByIssue"}, run: checkConcurrentLocalCommentReplies},
	{name: "comment read graph", methods: []string{"CreateProject", "CreateIssue", "CreateComment", "ReadCommentGraph", "IssueByID", "MoveIssueProject", "PurgeIssue"}, run: checkCommentReadGraph},
	{name: "screen view claims", methods: []string{"ClaimScreenView", "ReleaseScreenView"}, run: checkScreenViewClaims},
	{name: "remote event batch insert", methods: []string{"InsertRemoteEvents", "AdoptProjectIntoFederation", "MaterializeFederatedProject"}, runWithBackend: checkRemoteEventBatchInsert},
	{name: "pushed definition authorship", methods: []string{"PutCronWorkflow", "IngestFederationEvents", "CronWorkflow"}, run: checkPushedDefinitionAuthorship},
	{name: "run actor survives push enablement", methods: []string{"ObserveCronRun", "AdoptProjectIntoFederation", "EnableFederationPush"}, run: checkRunActorSurvivesPushEnablement},
	{name: "hub accepts known run actor", methods: []string{"ObserveCronRun", "IngestFederationEvents"}, run: checkHubAcceptsKnownRunActor},
	{name: "cron reads tolerate tightened rules", methods: []string{"PutCronJob", "CronJob", "ListCronJobs", "ExportCronJobs"}, run: checkCronReadsTolerateTightenedRules},
	{name: "federation metadata requires enabled hub", methods: []string{"CreateProject", "AdoptProjectIntoFederation", "ReadFederation"}, run: checkFederationMetadataRequiresEnabledHub},
	{name: "peer ingest committed events", methods: []string{"IngestFederationEvents"}, run: checkPeerIngestCommittedEvents},
	{name: "independent run observations", methods: []string{"ObserveCronRun", "CronRun", "ListCronRuns", "ExportCronJobs", "ExportCronWorkflows", "ExportCronRuns"}, run: checkIndependentRunObservations},
	{name: "cron definition export deleted filter", methods: []string{"CreateProject", "PutCronJob", "PutCronWorkflow", "ObserveCronRun", "ExportCronJobs", "ExportCronWorkflows", "ExportCronRuns"}, run: checkCronDefinitionExportDeletedFilter},
	{name: "cron data blocks orphan cleanup", methods: []string{"CreateProject", "HardDeleteProject", "PutCronJob", "ObserveCronRun", "CronJob", "CronRun", "ProjectByID"}, run: checkCronDataBlocksOrphanCleanup},
	{name: "cron run revision after remote winner", methods: []string{"CreateProject", "PutCronJob", "ObserveCronRun", "EnableProjectFederation", "InsertRemoteEvent", "MaterializeFederatedProject", "CronRun"}, run: checkCronRunRevisionAfterRemoteWinner},
	{name: "run history instants", methods: []string{"ObserveCronRun", "ListCronRuns"}, run: checkRunHistoryInstants},
	{name: "cron backup tombstone round trip", methods: []string{"CreateProject", "PutCronJob", "PutCronWorkflow", "ObserveCronRun", "ImportReplay", "CronJob", "CronWorkflow", "CronRun", "ListCronJobs", "ListCronWorkflows"}, runWithBackend: checkCronBackupTombstoneRoundTrip},
	{name: "cron project purge", methods: []string{"CreateProject", "PutCronJob", "PutCronWorkflow", "ObserveCronRun", "RemoveProject", "PurgeProject", "CronJob", "CronWorkflow", "CronRun"}, run: checkCronProjectPurge},
	{name: "run observation validation", methods: []string{"CreateProject", "PutCronJob", "PutCronWorkflow", "ObserveCronRun", "CronRun"}, run: checkRunObservationValidation},
	{name: "run reference restore", methods: []string{"ImportReplay"}, runWithBackend: checkRunReferenceRestore},
	{name: "run reference federation", methods: []string{"InsertRemoteEvent", "IngestFederationEvents"}, run: checkRunReferenceFederation},
	{name: "run compacted provenance", methods: []string{"ResetFederatedProject", "MaterializeFederatedProject"}, runWithBackend: checkRunCompactedProvenance},
	{name: "issue planning dates", methods: []string{"IssuePlanningDates"}, run: checkIssuePlanningDates},
	{name: "native cron remote validation", methods: []string{}, runWithBackend: checkNativeCronValidation},
	{name: "native cron adoption author", methods: []string{}, runWithBackend: checkNativeCronAdoptionAuthor},
	{name: "native cron federation", methods: []string{"ReadFederation"}, runWithBackend: checkNativeCronFederation},
	{name: "native cron push", methods: []string{}, runWithBackend: checkNativeCronPush},
	{name: "native cron definitions", methods: []string{"PutCronJob", "PutCronWorkflow", "CronJob", "CronWorkflow", "ListCronJobs", "ListCronWorkflows"}, run: checkNativeCronDefinitions},
	{name: "native cron constraints", methods: []string{}, run: checkNativeCronConstraints},
	{name: "native cron dormant storage", methods: []string{}, run: checkNativeCronDormancy},
	{name: "cron project merge restore", methods: []string{"CreateProject", "PutCronJob", "PutCronWorkflow", "ObserveCronRun", "MergeProjects", "ImportReplay"}, runWithBackend: checkCronProjectMergeRestore},
	{name: "purge attribution and gate", methods: []string{"PurgeIssue", "EntityAttribution", "AttributionReceiptsAfter"}, run: func(t *testing.T, store db.Storage) error {
		RunPurgeAttributionAndGate(t, store)
		return nil
	}},
	{name: "pending creation lifecycle", methods: []string{"ApplyUpstreamAttribution", "ExportMeta", "LeaveFederationReplica", "PurgeIssue", "PurgeProject", "SetRelayBindingConfig"}, run: func(t *testing.T, store db.Storage) error {
		RunPendingCreationLifecycle(t, store)
		return nil
	}},
	{name: "relay reset drops orphan pending creations", methods: []string{"ExportMeta", "PinRootAuthority", "SetRelayBindingConfig"}, run: func(t *testing.T, store db.Storage) error {
		RunRelayResetDropsOrphanPendingCreations(t, store)
		return nil
	}},
	{name: "project merge access and artifacts", methods: []string{"MergeProjects", "SetProjectAccessPolicy"}, run: func(t *testing.T, store db.Storage) error {
		RunProjectMergeAccessAndArtifacts(t, store)
		return nil
	}},
	{name: "project merge duplicate artifact", methods: []string{"MergeProjects", "MoveIssueProject"}, run: func(t *testing.T, store db.Storage) error {
		RunProjectMergeDuplicateArtifact(t, store)
		return nil
	}},
	{name: "project merge artifact retention", methods: []string{"MergeProjects"}, runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
		RunProjectMergeArtifactRetention(t, store, backend)
		return nil
	}},

	{name: "external import derived status", methods: []string{"CreateProject", "ImportBatch", "ImportMappingBySource", "IssueByID", "EditIssue"}, run: checkImportDerivedStatus},
	{name: "external import status expiry after mode switch", methods: []string{"ClaimOwner", "CreateProject", "EventsAfter", "ExpireAssignments", "ImportBatch", "ImportMappingBySource", "IssueByID"}, run: checkImportStatusCloseClearsAssignmentExpiry},
	{name: "external import status observation acknowledgement", methods: []string{"CreateProject", "ImportBatch", "ImportMappingBySource", "IssueByID", "ReopenIssue"}, run: checkImportStatusObservationAcknowledgement},
	{name: "external import status keeps local closure", methods: []string{"CloseIssueWithEvents", "CreateProject", "ImportBatch", "ImportMappingBySource", "IssueByID"}, run: checkImportStatusKeepsLocalClosure},
	{name: "issue status federation intent", methods: []string{"IngestFederationEvents", "MaterializeFederatedProject", "CreateIssue", "UpsertIssueSyncBinding"}, run: checkIssueStatusFederationIntent},
	{name: "issue status native intent", methods: []string{"CloseIssueWithEvents", "ReopenIssue", "CreateIssue", "UpsertIssueSyncBinding"}, runWithBackend: checkIssueStatusNativeIntent},
	{name: "issue status mapping reads", methods: []string{"CreateProject", "CreateIssue", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding"}, run: checkIssueStatusMappingReads},
	{name: "issue status mapping storage", methods: []string{"CreateProject", "CreateIssue", "UpsertImportMapping"}, run: checkIssueStatusMappingStorage},
	{name: "issue status observation", methods: []string{"CreateProject", "CreateIssue", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding"}, runWithBackend: checkIssueStatusObservation},
	{name: "issue status content isolation", methods: []string{"CreateProject", "ImportBatch", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding"}, run: checkIssueStatusContentIsolation},
	{name: "issue status close metadata", methods: []string{"CreateProject", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding", "UpsertImportMapping"}, run: checkIssueStatusProviderCloseMetadata},
	{name: "issue status archive fence", methods: []string{"CreateProject", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding", "RemoveProject", "RestoreProject"}, run: checkIssueStatusArchiveFence},
	{name: "issue status locator", methods: []string{"CreateIssue", "CreateProject", "CloseIssue", "ClaimIssueSyncBinding", "SoftDeleteIssue", "UpsertIssueSyncBinding", "UpsertImportMapping"}, runWithBackend: checkIssueStatusLocator},
	{name: "issue move old refs", methods: []string{"MoveIssueProject"}, run: checkMoveOldRefs},
	{name: "issue move reused old refs", methods: []string{"CreateProject", "CreateIssue", "MoveIssueProject", "IssueByShortID", "IssueByUID", "SoftDeleteIssue", "RestoreIssue", "PurgeIssue"}, run: checkMoveReusedOldRefs},
	{name: "issue move tree external claim", methods: []string{"MoveIssueProject"}, run: checkMoveTreeExternalClaim},
	{name: "issue move tree expanded fence", methods: []string{"MoveIssueProject"}, run: checkMoveTreeExpandedFence},
	{name: "issue move tree mapping collision", methods: []string{"MoveIssueProject"}, run: checkMoveTreeMappingCollision},
	{name: "issue move merge reservations", methods: []string{"MoveIssueProject", "MergeProjects"}, run: checkMergeMoveReservations},
	{name: "issue move tree", methods: []string{"MoveIssueProject"}, run: checkMoveTree},
	{name: "issue move tree guards", methods: []string{"MoveIssueProject"}, run: checkMoveTreeGuards},
	{name: "issue move tree stops at deleted issue", methods: []string{"MoveIssueProject", "SoftDeleteIssue"}, run: checkMoveTreeStopsAtDeletedIssue},
	{name: "close rechecks evidence targets", methods: []string{"CloseIssueGuarded", "SoftDeleteIssue"}, run: checkCloseRechecksEvidenceTargets},
	{name: "issue move tree archived descendant", methods: []string{"MoveIssueProject", "RemoveProject"}, run: checkMoveTreeArchivedDescendant},
	{name: "issue status move", methods: []string{"CreateProject", "CreateIssue", "CloseIssueWithEvents", "MoveIssueProject", "UpsertIssueSyncBinding", "UpsertImportMapping"}, run: checkIssueStatusMove},
	{name: "issue status settings keep content cursor", methods: []string{"CreateProject", "UpsertIssueSyncBinding"}, run: checkIssueStatusSettingsKeepContentCursor},
	{name: "issue status page isolates invalid mapping", methods: []string{"CreateProject", "CreateIssue", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding", "UpsertImportMapping"}, run: checkIssueStatusPageIsolatesInvalidMapping},
	{name: "issue status claim recovery", methods: []string{"CreateProject", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding", "DisableIssueSyncBinding"}, run: checkIssueStatusClaimRecovery},
	{name: "external import presentation labels stale replay", methods: []string{"CreateProject", "ImportBatch", "ImportMappingBySource", "IssueByID", "LabelsByIssue", "EditIssue"}, run: checkImportPresentationLabelStaleReplay},
	{name: "external import presentation labels", methods: []string{"CreateProject", "ImportBatch", "ImportMappingBySource", "IssueByID", "LabelsByIssue", "AddLabel", "RemoveLabelAndEvent", "EditIssue"}, run: checkImportPresentationLabels},
	{
		name:    "issue revision owner",
		methods: []string{"ClaimOwner", "CreateIssue", "CreateProject", "IssueByID", "PatchIssueMetadata", "UnassignOwner", "UpdateOwner"},
		run:     checkIssueRevisionOwner,
	},
	{
		name:    "issue revision edits",
		methods: []string{"CreateIssue", "CreateProject", "EditIssue", "EditIssueAtomic", "IssueByID"},
		run:     checkIssueRevisionEdits,
	},
	{
		name:    "issue revision status",
		methods: []string{"CloseIssue", "CreateIssue", "CreateProject", "IssueByID", "PatchIssueMetadata", "ReopenIssue"},
		run:     checkIssueRevisionStatus,
	},
	{
		name:    "issue revision import",
		methods: []string{"CreateProject", "ImportBatch", "ImportMappingBySource", "IssueByID"},
		run:     checkIssueRevisionImport,
	},
	{
		name:    "issue revision author rewrite",
		methods: []string{"CreateIssue", "CreateProject", "IssueByID", "RewriteAuthorIdentity"},
		run:     checkIssueRevisionAuthorRewrite,
	},
	{name: "search status", methods: []string{"SearchFTS", "SearchFTSAny"}, run: checkSearchStatus},
	{name: "restricted search reads", methods: []string{"AddLabel", "CreateComment", "CreateIssue", "CreateProject", "SearchFTS", "SearchFTSAny"}, run: checkRestrictedSearchReads},
	{
		name:    "search assignment expiry",
		methods: []string{"ClaimOwner", "CreateIssue", "CreateProject", "SearchFTS", "SearchFTSAny"},
		run:     checkSearchRoundTripsAssignmentExpiry,
	},
	{
		name:    "list ordering",
		methods: []string{"CreateProject", "ImportBatch", "ImportMappingBySource", "ListAllIssues", "ListIssues"},
		run:     checkListOrdering,
	},
	{
		name:    "lifecycle",
		methods: []string{"InstanceUID", "Path", "RefreshInstanceUID", "RetryTransient", "SchemaVersion"},
		run:     checkLifecycle,
	},
	{
		name:    "instance creation time",
		methods: []string{"ImportReplay", "InstanceCreatedAt", "InstanceUID"},
		run:     checkInstanceCreatedAt,
	},
	{
		name: "projects",
		methods: []string{
			"CreateProject", "CreateProjectAndEvent", "CreateProjectWithUID", "CreateProjectWithUIDAndEvent",
			"ListProjects", "ListProjectsIncludingArchived", "ProjectByID", "ProjectByName",
			"ProjectByNameIncludingArchived", "ProjectByUID", "RenameProject", "RenameProjectAndEvent",
		},
		run: checkProjects,
	},
	{
		name: "issue event atomicity",
		methods: []string{
			"CreateIssue", "CreateProject", "EventsAfter", "IssueByID", "IssueByShortID", "IssueByUID",
			"ListIssues", "MaxEventID",
		},
		run: checkIssueEventAtomicity,
	},
	{
		name: "project aliases",
		methods: []string{
			"AliasByID", "AliasByIdentity", "AttachAlias", "CreateProject", "HardDeleteProject",
			"LatestAliasForProject", "ProjectAliases", "ProjectByID", "ReassignAlias",
		},
		run: checkProjectAliases,
	},
	{
		name:    "idempotency",
		methods: []string{"AcquireIdempotencyLock", "CreateComment", "CreateIssue", "CreateProject", "LookupCommentIdempotency", "LookupIdempotency", "LookupIssueMutationIdempotency"},
		run:     checkIdempotency,
	},
	{
		name:    "historical close receipts",
		methods: []string{"CreateIssue", "CreateProject", "CloseIssueGuarded", "MoveIssueProject", "SoftDeleteIssue", "LookupIssueMutationIdempotencyByIssue"},
		run:     checkHistoricalCloseReceipts,
	},
	{
		name:    "concurrent uniqueness",
		methods: []string{"CreateProject", "ListProjects"},
		run:     checkConcurrentUniqueness,
	},
	{
		name: "purge reset",
		methods: []string{
			"CreateIssue", "CreateProject", "ExportPurgeLog", "ExportSequences",
			"IssueByUID", "PurgeIssue", "PurgeResetCheck",
		},
		run: checkPurgeReset,
	},
	{
		name: "purge preserves signed peer event",
		methods: []string{
			"ApplyUpstreamAttribution", "CreateIssue", "CreateLinkAndEvent", "CreateProject",
			"ExportAttribution", "ExportEvents", "ImportReplay", "LeaveFederationReplica",
			"PurgeIssue", "PinRootAuthority", "UpsertFederationBinding",
		},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			RunPurgePreservesSignedPeerEvent(t, store, backend)
			return nil
		},
	},
	{
		name: "project purge preserves signed peer event",
		methods: []string{
			"ApplyUpstreamAttribution", "CreateIssue", "CreateLinkAndEvent", "CreateProject",
			"ExportAttribution", "ExportEvents", "ImportReplay", "LeaveFederationReplica",
			"PurgeProject", "PinRootAuthority", "RemoveProject", "UpsertFederationBinding",
		},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			RunProjectPurgePreservesSignedPeerEvent(t, store, backend)
			return nil
		},
	},
	{
		name: "live-only backup preserves signed soft-deleted peer",
		methods: []string{
			"ApplyUpstreamAttribution", "CreateIssue", "CreateLinkAndEvent", "CreateProject",
			"ExportAttribution", "ExportEvents", "ImportReplay", "PinRootAuthority",
			"SoftDeleteIssue", "UpsertFederationBinding",
		},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			RunLiveOnlyBackupPreservesSignedSoftDeletedPeer(t, store, backend)
			return nil
		},
	},
	{
		name: "project purge removes relay metadata",
		methods: []string{
			"CreateProject", "ExportAttribution", "ExportMeta", "ExportProjects", "ExportEvents",
			"ImportReplay", "LeaveFederationReplica", "PinRootAuthority",
			"PurgeProject", "RemoveProject", "RotateRootAuthority", "SetRelayBindingConfig",
			"UpsertFederationBinding",
		},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			RunProjectPurgeRemovesRelayMetadata(t, store, backend)
			return nil
		},
	},
	{
		name: "project merge removes relay metadata",
		methods: []string{
			"CreateProject", "ExportAttribution", "ExportMeta", "ExportProjects", "ExportEvents",
			"ImportReplay", "LeaveFederationReplica", "MergeProjects",
			"PinRootAuthority", "RotateRootAuthority", "SetRelayBindingConfig", "UpsertFederationBinding",
		},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			RunProjectMergeRemovesRelayMetadata(t, store, backend)
			return nil
		},
	},
	{
		name: "archived detached relay metadata retains root keys",
		methods: []string{
			"CreateProject", "ExportAttribution", "LeaveFederationReplica",
			"PinRootAuthority", "RemoveProject", "RotateRootAuthority", "SetRelayBindingConfig",
			"UpsertFederationBinding",
		},
		run: func(t *testing.T, store db.Storage) error {
			RunArchivedDetachedRelayMetadataRetainsRootKeys(t, store)
			return nil
		},
	},
	{
		name:    "archived detached receipt export retains root keys",
		methods: []string{"ApplyUpstreamAttribution", "CreateIssue", "CreateProject", "ExportAttribution", "ImportReplay", "LeaveFederationReplica", "PinRootAuthority", "RemoveProject", "UpsertFederationBinding"},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			RunArchivedDetachedReceiptExport(t, store, backend)
			return nil
		},
	},
	{
		name: "project lifecycle",
		methods: []string{
			"AttachAlias", "CountOpenIssues", "CreateIssue", "CreateProject", "DetachProjectAlias",
			"ExportProjectPurgeLog", "ProjectAliases", "ProjectByID", "PurgeProject", "PurgeResetCheck",
			"RemoveProject", "RestoreProject",
		},
		run: checkProjectLifecycle,
	},
	{
		name: "issue lifecycle",
		methods: []string{
			"BatchProjectStats", "ClaimOwner", "CloseIssue", "CloseIssueGuarded", "CloseIssueWithEvents", "CreateIssue", "CreateProject", "EditIssue",
			"IssueByShortID", "IssueUIDPrefixMatch", "ListAllIssues", "ReopenIssue", "RestoreIssue",
			"SoftDeleteIssue", "UpdateOwner", "UpdatePriority",
		},
		run: checkIssueLifecycle,
	},
	{
		name: "event queries",
		methods: []string{
			"CreateIssue", "CreateProject", "EventsByUIDs", "EventsInWindow", "MaxFederationBaselineEventID",
			"MaxLocalOriginEventID",
		},
		run: checkEventQueries,
	},
	{
		name: "event reference project scope",
		methods: []string{
			"CloseIssueWithEvents", "CreateIssue", "CreateLink", "CreateProject", "EventsAfter", "MoveIssueProject",
			"EventsInWindow", "InsertCloseThrottledEvent", "InsertRemoteEvent",
		},
		run: func(t *testing.T, store db.Storage) error {
			RunEventReferenceProjectScope(t, store)
			return nil
		},
	},
	{
		name: "event release exposure",
		methods: []string{
			"CloseIssueWithEvents", "CreateIssue", "CreateLinkAndEvent", "CreateProject", "EventsAfter", "EventsInWindow",
			"InsertRemoteEvent", "MoveIssueProject",
		},
		run: func(t *testing.T, store db.Storage) error {
			RunEventReleaseExposure(t, store)
			return nil
		},
	},
	{
		name:    "uid-only event subject out of reach",
		methods: []string{"CreateIssue", "CreateProject", "EventsAfter", "MoveIssueProject"},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			RunUIDOnlyEventSubjectOutOfReach(t, store, backend)
			return nil
		},
	},
	{
		name:    "archived federation root pin export",
		methods: []string{"CreateProject", "ExportAttribution", "PinRootAuthority", "RemoveProject", "UpsertFederationBinding"},
		run: func(t *testing.T, store db.Storage) error {
			RunArchivedFederationPinExport(t, store)
			return nil
		},
	},
	{
		name: "archive and leave avoids relay debt",
		methods: []string{
			"CreateProject", "FederationBindingByProject", "LeaveFederationReplica", "PinRootAuthority", "RemoveProject",
			"SetRelayBindingConfig", "UpsertFederationBinding",
		},
		run: func(t *testing.T, store db.Storage) error {
			RunArchiveAndLeaveDoesNotCreateRelayDebt(t, store)
			return nil
		},
	},
	{
		name: "issue create envelope",
		methods: []string{
			"CreateIssue", "CreateProject", "ListIssues", "MaxEventID",
		},
		run: checkIssueCreateEnvelope,
	},
	{
		name:    "concurrent issue identity",
		methods: []string{"CreateIssue", "CreateProject"},
		run:     checkConcurrentIssueIdentity,
	},
	{
		name:    "concurrent owner claim",
		methods: []string{"ClaimOwner", "CreateIssue", "CreateProject"},
		run:     checkConcurrentOwnerClaim,
	},
	{
		name:    "concurrent guarded owner claim",
		methods: []string{"ClaimOwner", "CreateIssue", "CreateProject", "IssueByID"},
		run:     checkConcurrentGuardedOwnerClaim,
	},
	{
		name:    "timed assignment owner mutations",
		methods: []string{"ClaimOwner", "CreateIssue", "CreateProject", "EditIssue", "EditIssueAtomic", "UnassignOwner", "UpdateOwner"},
		run:     checkTimedAssignmentOwnerMutations,
	},
	{
		name:    "bounded assignment expiry",
		methods: []string{"ClaimOwner", "CloseIssue", "CreateIssue", "CreateProject", "ExpireAssignments", "IssueByID"},
		run:     checkBoundedAssignmentExpiry,
	},
	{
		name:    "expected owner unassign",
		methods: []string{"CreateIssue", "CreateProject", "UnassignOwner"},
		run:     checkExpectedOwnerUnassign,
	},
	{
		name:    "concurrent expected owner unassign",
		methods: []string{"CreateIssue", "CreateProject", "IssueByID", "UnassignOwner", "UpdateOwner"},
		run:     checkConcurrentExpectedOwnerUnassign,
	},
	{
		name: "comments",
		methods: []string{
			"CommentBodyByID", "CommentsByIssue", "CreateComment", "CreateIssue", "CreateProject",
			"EditComment", "IssueByID", "RewriteAuthorIdentity",
		},
		run: checkComments,
	},
	{
		name: "labels",
		methods: []string{
			"AddLabel", "AddLabelAndEvent", "CreateIssue", "CreateProject", "HasLabel", "LabelByEndpoints",
			"LabelCounts", "LabelsByIssue", "LabelsByIssues", "LabelsForIssue", "RemoveLabel",
			"RemoveLabelAndEvent", "SoftDeleteIssue",
		},
		run: checkLabels,
	},
	{
		name: "links and relationship projections",
		methods: []string{
			"ChildrenOfIssue", "CloseIssue", "CreateIssue", "CreateLink",
			"CreateLinkAndEvent", "CreateProject", "DeleteLinkAndEvent", "DeleteLinkByID",
			"LinkByEndpoints", "LinkByID", "LinksByIssue", "OpenChildrenOf", "ParentOf",
			"ParentShortIDsByIssues",
		},
		run: checkLinksAndRelationshipProjections,
	},
	{
		name: "issue relationships",
		methods: []string{
			"CreateIssue", "CreateLink", "CreateProject", "RelationshipsByIssues",
			"RemoveProject", "SoftDeleteIssue",
		},
		run: checkIssueRelationships,
	},
	{
		name: "restricted relationship reads",
		methods: []string{
			"ChildrenOfIssue", "CreateIssue", "CreateLink", "CreateProject", "ParentShortIDsByIssues",
			"ReadyIssues", "ReadyIssuesGlobal", "RelationshipsByIssues",
		},
		run: checkRestrictedRelationshipReads,
	},
	{
		name: "archived link targets",
		methods: []string{
			"CreateIssue", "CreateLink", "CreateLinkAndEvent", "CreateProject", "EditIssueAtomic",
			"IssueByID", "LinksByIssue", "RemoveProject",
		},
		run: checkArchivedLinkTargets,
	},
	{
		name:    "ready assignment expiry",
		methods: []string{"CreateProject", "CreateIssue", "ClaimOwner", "ReadyIssues", "ReadyIssuesGlobal", "IssueByID", "MaxEventID", "PatchIssueMetadata", "AddLabel", "CreateLink"},
		run:     checkReadyAssignmentExpiry,
	},
	{
		name: "ready queues and discovery",
		methods: []string{
			"AddLabel", "CloseIssue", "CreateComment", "CreateIssue", "CreateLink", "CreateProject", "EditIssue",
			"IssueQualifiersByUIDs", "ListAllIssues", "ListIssueContent", "PatchIssueMetadata", "ReadyIssues", "ReadyIssuesGlobal", "SearchFTS",
			"SearchFTSAny", "SoftDeleteIssue",
		},
		run: checkReadyQueuesAndDiscovery,
	},
	{
		name: "api tokens and system project",
		methods: []string{
			"CreateAPIToken", "CreateComment", "CreateIssue", "CreateProject", "EnsureSystemProject",
			"APITokenByID", "IssueInScope", "IssueScopedMembers", "IssueScopedTokenTransactionFence", "ListAPITokens", "ListProjects", "ProjectByID",
			"ResolveAPIToken", "RevokeAPIToken", "SystemProject",
		},
		run: checkAPITokensAndSystemProject,
	},
	{
		name: "recurrences",
		methods: []string{
			"CloseIssueWithEvents", "CreateIssue", "CreateProject", "CreateRecurrence", "CreateRecurrenceForIssue", "EventsAfter", "GetRecurrenceByID",
			"GetRecurrenceByUID", "IssueByID", "LabelsForIssue", "ListIssues", "ListRecurrencesByProject",
			"MaterializeNext", "PatchRecurrence", "PurgeIssue", "SoftDeleteRecurrence",
		},
		run: checkRecurrences,
	},
	{
		name: "close audit queries",
		methods: []string{
			"CloseIssue", "CreateIssue", "CreateLink", "CreateProject", "InsertCloseThrottledEvent",
			"RecentSameMessageClose", "RecentSiblingCloses", "RemoveProject",
		},
		run: checkCloseAuditQueries,
	},
	{
		name:    "issue_sync_expected_binding",
		methods: []string{"CreateProject", "UpsertIssueSyncBinding", "IssueSyncBindingByProject"},
		run:     checkIssueSyncExpectedBinding,
	},
	{
		name:    "issue_status_content_warning",
		methods: []string{"CreateProject", "UpsertIssueSyncBinding", "ClaimIssueSyncBinding", "RecordIssueSyncSuccess", "IssueSyncBindingByID", "IssueSyncStatusByProject"},
		run:     checkIssueStatusContentWarning,
	},
	{
		name: "issue sync lifecycle",
		methods: []string{
			"ClaimIssueSyncBinding", "CreateProject", "DisableIssueSyncBinding", "IssueSyncBindingByID",
			"IssueSyncBindingByProject", "IssueSyncStatusByProject", "ListDueIssueSyncBindings",
			"RecordIssueSyncError", "RecordIssueSyncSuccess", "RefreshIssueSyncBinding", "UpsertIssueSyncBinding",
		},
		run: checkIssueSyncLifecycle,
	},
	{
		name: "notion_sync",
		methods: []string{
			"ClaimIssueSyncBinding", "CreateProject", "DisableIssueSyncBinding", "IssueSyncBindingByID",
			"IssueSyncBindingByProject", "IssueSyncStatusByProject", "ListDueIssueSyncBindings",
			"RecordIssueSyncError", "RecordIssueSyncSuccess", "RefreshIssueSyncBinding", "UpsertIssueSyncBinding",
			"ImportBatch", "UpsertFederationBinding", "CreateExternalRootBinding", "RemoveProject",
			"CreateIssue", "UpsertImportMapping", "ImportMappingBySource", "PauseExternalRootBinding",
		},
		run: checkNotionSyncStorage,
	},
	{
		name: "metadata and atomic edit",
		methods: []string{
			"CreateIssue", "CreateProject", "DesignateInboxProject", "EditIssueAtomic", "EventsAfter", "IssueByID",
			"LinkByEndpoints", "ListIssueContent", "PatchIssueMetadata", "PatchProjectMetadata", "ProjectByID",
		},
		run: checkMetadataAndAtomicEdit,
	},
	{
		name: "due notifications",
		methods: []string{
			"CreateIssue", "CreateProject", "IssueByID", "ListDueNotificationIssueIDs", "ReconcileDueNotification",
		},
		run: checkDueNotifications,
	},
	{
		name: "scheduled reached",
		methods: []string{
			"CloseIssue", "CreateIssue", "CreateProject", "EventsByUIDs", "IssueByID", "PatchIssueMetadata",
			"ReconcileDueNotification",
		},
		run: checkScheduledReached,
	},
	{
		name: "scheduled reached floor",
		methods: []string{
			"CreateIssue", "CreateProject", "CreateRecurrenceForIssue", "EnableProjectFederation",
			"EnsureScheduledReachedFloor", "InsertRemoteEvent", "PatchIssueMetadata", "ReconcileDueNotification",
		},
		run: checkScheduledReachedFloor,
	},
	{
		name: "due notification timezones",
		run:  checkDueNotificationTimezones,
	},
	{
		name: "due notification federated clear",
		run:  checkDueNotificationFederatedClear,
	},
	{
		name: "import mappings",
		methods: []string{
			"AddLabel", "CreateComment", "CreateIssue", "CreateLink", "CreateProject",
			"ImportMappingBySource", "ImportMappingsByProjectSource", "UpsertImportMapping",
		},
		run: checkImportMappings,
	},
	{
		name: "external root durable state",
		methods: []string{
			"ClaimExternalRootBinding", "ClaimExternalRootBindingForManualAction", "ClaimExternalRootBindingForManualReconcile",
			"ClearPendingExternalComment", "CreateExternalRootBinding",
			"CreateComment", "CreateIssue", "CreateProject", "CreateProjectWithUID", "EnableProjectFederation",
			"ExternalFieldStates", "ExternalRootBindingByExternalKey", "ExternalRootBindingByID",
			"ExternalRootBindingByIssue", "ImportCommentMappingsByIssue", "ImportMappingBySource", "IngestFederationEvents",
			"IssueByUID", "ListDueExternalRootBindings", "ListExternalFieldMappings",
			"PauseExternalRootBinding", "PendingFederationPushEvents", "RecordExternalRootError",
			"RecordExternalRootSuccess", "ReleaseExternalRootClaim", "RenewExternalRootClaim", "ResolveExternalFieldConflict",
			"ResumeExternalRootBinding", "SetPendingExternalComment", "UnbindExternalRootBinding",
			"UnmapExternalField", "UpsertExternalFieldMapping", "UpsertExternalFieldState",
		},
		runWithBackend: checkExternalRootBindings,
	},
	{
		name: "external root safety invariants",
		methods: []string{
			"ApplyExternalFieldProjection", "ApplyExternalRootProjection", "ClaimExternalRootBinding", "ClearPendingExternalComment", "CreateComment", "CreateLink",
			"CloseIssue", "ExternalRootBindingByID", "EnsureExternalRootLifecycleRequest", "HasLabel", "ImportMappingBySource", "IssueByID",
			"ListDueExternalRootBindings", "ListExternalFieldMappings", "PauseExternalRootBinding", "ReleaseExternalRootClaim", "RemoveLabelAndEvent", "RemoveProject",
			"ImportReplay", "ReopenIssue", "ResolveExternalFieldConflict", "RestoreProject", "ResumeExternalRootBinding", "SetPendingExternalComment", "UpsertExternalCommentProjection",
			"DeleteLinkByID", "SoftDeleteIssue", "UnbindExternalRootBinding", "UnmapExternalField", "UpsertExternalFieldMapping", "UpsertExternalFieldState", "UpsertFederationBinding", "UpsertImportMapping",
		},
		runWithBackend: checkExternalRootSafetyInvariants,
	},
	{
		name: "external root content ownership",
		methods: []string{
			"ApplyExternalRootProjection", "CreateExternalRootBinding", "CreateIssue", "CreateProject", "CreateProjectWithUID",
			"EditIssue", "EditIssueAtomic", "EnableProjectFederation", "ImportBatch",
			"ImportMappingBySource", "IngestFederationEvents", "IssueByID", "MaxEventID",
			"PauseExternalRootBinding", "UnbindExternalRootBinding", "UpsertImportMapping", "UpsertIssueSyncBinding",
		},
		runWithBackend: checkExternalRootContentOwnership,
	},
	{
		name: "project relocation",
		methods: []string{
			"AcquireClaim", "ClaimStatusReadOnly", "CountLiveClaims", "CountPendingClaims", "CreateIssue",
			"ClaimExternalRootBinding", "CreateExternalRootBinding", "CreateLink", "CreateProject", "CreateRecurrence", "EnqueuePendingClaim",
			"EventsAfter", "ExternalRootBindingByID", "ImportMappingBySource", "IssueByID", "LinkByEndpoints",
			"ListPendingClaimRequestsForIssue", "MaterializeNext", "MoveIssueProject", "RemoveProject", "UnbindExternalRootBinding",
			"MaxEventID", "ReleaseExternalRootClaim", "UpsertFederationBinding", "UpsertImportMapping", "UpsertIssueSyncBinding",
		},
		run: checkProjectRelocation,
	},
	{
		name: "project merge",
		methods: []string{
			"AttachAlias", "ClaimExternalRootBinding", "CreateExternalRootBinding", "CreateIssue", "CreateLink", "CreateProject",
			"CreateRecurrence", "EventsAfter", "ExternalRootBindingByID", "ImportMappingBySource",
			"IssueByID", "IssueByShortID", "LinksByIssue", "MergeProjects", "ProjectAliases",
			"ListRecurrencesByProject", "ProjectByID", "PurgeIssue", "ReleaseExternalRootClaim", "RemoveProject", "SystemProject",
			"UpsertImportMapping", "UpsertIssueSyncBinding",
		},
		run: checkProjectMerge,
	},
	{
		name: "active projection exports",
		methods: []string{
			"AddLabel", "AttachAlias", "CreateComment", "CreateIssue", "CreateLink", "CreateProject",
			"CreateRecurrence", "ExportComments", "ExportEvents", "ExportImportMappings", "ExportIssueLabels",
			"ExportIssueSyncBindings", "ExportIssueSyncStatus", "ExportIssues", "ExportLinks", "ExportMeta",
			"ExportProjectAliases", "ExportProjects", "ExportRecurrences", "SoftDeleteIssue", "UpsertImportMapping",
			"UpsertIssueSyncBinding",
		},
		run: checkActiveProjectionExports,
	},
	{
		name: "claim lease lifecycle",
		methods: []string{
			"AcquireClaim", "ClaimStatus", "ClaimStatusReadOnly", "CloseIssueWithEvents", "CountLiveClaims",
			"CountPendingClaims", "CreateIssue", "CreateProject", "EnableProjectFederation",
			"ExpireTimedClaims", "ExpireTimedClaimsForProject",
			"ForceReleaseClaim", "ReleaseClaim", "RenewClaim", "SoftDeleteIssue",
		},
		run: checkClaimLeaseLifecycle,
	},
	{
		name: "pending claim and cache lifecycle",
		methods: []string{
			"ApplyClaimStatus", "CheckClaimGate", "ClaimStatusReadOnly", "ClaimStatusRefreshError",
			"ClearClaimStatusRefreshError", "CountPendingClaims", "CreateIssue", "CreateProject",
			"EnqueuePendingClaim", "ExportIssueClaims", "ExportPendingClaimRequests",
			"ListPendingClaimRequests", "ListPendingClaimRequestsForIssue",
			"MarkClaimStatusRefreshError", "MarkPendingClaimAttempt", "RejectPendingClaim",
			"ResolvePendingClaim", "SoftDeleteIssue", "UpsertClaimCache",
		},
		runWithBackend: checkPendingClaimAndCacheLifecycle,
	},
	{
		name: "claim violation queries",
		methods: []string{
			"AcquireClaim", "CreateIssue", "CreateProject", "ReleaseClaim",
			"UnresolvedClaimViolationsForIssue", "UnresolvedClaimViolationsForProject",
		},
		runWithBackend: checkClaimViolationQueries,
	},
	{
		name: "federation control lifecycle",
		methods: []string{
			"ActiveFederationQuarantine", "ActiveFederationQuarantinesByProject",
			"AdvanceFederationPullCursor", "AdvanceFederationPushCursor", "AuthorizeFederationToken",
			"ClearFederationSyncError", "CloseIssueWithEvents", "CountActiveFederationEnrollments", "CreateFederationEnrollment",
			"CreateProjectFederationEnrollment",
			"CreateIssue", "CreateLink", "CreateProject", "EnableFederationPush", "ExportFederationBindings",
			"ExportFederationEnrollments", "ExportFederationQuarantine", "ExportFederationSyncStatus",
			"FederationBindingByProject", "FederationEnrollmentTransactionFence",
			"FederationSyncStatusByProject", "FindActiveFederationEnrollment", "ListFederationBindings",
			"ListFederationEnrollments", "LinkByEndpoints", "PurgeIssue", "RecordFederationQuarantine", "RecordFederationSyncError",
			"RecordFederationSyncPullStarted", "RecordFederationSyncPullSuccess",
			"RecordFederationSyncPushStarted", "RecordFederationSyncPushSuccess",
			"RecordFederationSyncReset", "RetryFederationQuarantine", "RevokeFederationEnrollment",
			"SkipFederationQuarantine", "UpsertFederationBinding",
		},
		run: checkFederationControlLifecycle,
	},
	{
		name: "federation binding endpoint rebind",
		methods: []string{
			"CreateProject", "FederationBindingByProject", "RebindFederationBinding",
			"UpsertFederationBinding",
		},
		run: checkFederationBindingEndpointRebind,
	},
	{
		name: "federation enrollment rotation",
		methods: []string{
			"CreateFederationEnrollment", "CreateProject", "ListFederationEnrollments",
			"RotateFederationEnrollment",
		},
		runWithBackend: checkFederationEnrollmentRotation,
	},
	{
		name: "project federation enrollment scope",
		methods: []string{
			"CreateFederationEnrollment", "CreateProject", "ListFederationEnrollments",
			"ListProjectFederationEnrollments", "RevokeFederationEnrollment",
		},
		run: checkProjectFederationEnrollmentScope,
	},
	{
		name: "federation event transport",
		methods: []string{
			"AddLabelAndEvent", "CommentsByIssue", "CreateComment", "CreateIssue", "CreateLinkAndEvent",
			"CreateProject", "CreateProjectWithUID", "EditIssueAtomic", "EnableProjectFederation",
			"EventsByUIDs", "IngestFederationEvents", "InsertRemoteEvent", "IssueByUID",
			"LabelByEndpoints", "LinkByEndpoints", "PendingFederationPushEvents",
			"PendingFederationPushStats", "ReconcileLocalFederationEcho", "UpsertFederationBinding",
		},
		runWithBackend: checkFederationEventTransport,
	},
	{
		name: "federation reset lifecycle",
		methods: []string{
			"AcquireClaim", "CountLiveClaims", "CountPendingClaims", "CreateExternalRootBinding", "CreateIssue", "CreateProject",
			"EnqueuePendingClaim", "EventsAfter", "ExternalRootBindingByID", "FederationBindingByProject", "IssueByUID", "MaxEventID",
			"RecordFederationQuarantine", "ResetFederatedProject", "ResetFederatedProjectIfNoPendingPush",
			"UnbindExternalRootBinding", "UpsertFederationBinding",
		},
		runWithBackend: checkFederationResetLifecycle,
	},
	{
		name: "federation projection lifecycle",
		methods: []string{
			"AcquireClaim", "CommentsByIssue", "CountLiveClaims", "CreateComment", "CreateIssue",
			"CreateProject", "CreateProjectWithUID", "EnableProjectFederation", "EventsAfter",
			"FederationBindingByProject", "InsertRemoteEvent", "IssueByUID", "LabelsForIssue",
			"LeaveFederationReplica", "MaterializeFederatedProject", "MaxEventID", "ProjectByID",
			"RefreshProjectFederationBaseline", "UpsertFederationBinding",
		},
		run: checkFederationProjectionLifecycle,
	},
	{
		name: "federation teammate remote validation",
		methods: []string{
			"CreateProject", "EventsByUIDs", "InsertRemoteEvent", "IssueByUID",
			"MaterializeFederatedProject", "UpsertFederationBinding",
		},
		run: checkFederationTeammateRemoteValidation,
	},
	{
		name: "federation ingest lifecycle",
		methods: []string{
			"AcquireClaim", "CommentsByIssue", "CreateProject", "EnableProjectFederation",
			"EventsByUIDs", "IngestFederationEvents", "IssueByUID",
			"UnresolvedClaimViolationsForIssue",
		},
		run: checkFederationIngestLifecycle,
	},
	{
		name: "federation adoption ingest lifecycle",
		methods: []string{
			"CreateFederationEnrollment", "CreateProject", "EnableProjectFederation",
			"IngestFederationEvents", "IssueByUID", "ListFederationEnrollments",
		},
		run: checkFederationAdoptionIngestLifecycle,
	},
	{
		name: "federation snapshot link dates",
		methods: []string{
			"CreateProject", "CreateIssue", "CreateLink", "EnableProjectFederation",
			"EventsAfter", "DeleteLinkByID", "MaterializeFederatedProject", "LinkByEndpoints",
		},
		run: checkFederationSnapshotLinkDates,
	},
	{
		name: "federation project adoption",
		methods: []string{
			"AcquireClaim", "AdoptProjectIntoFederation", "CountLiveClaims", "CountPendingClaims",
			"CreateComment", "CreateExternalRootBinding", "CreateIssue", "CreateProject", "EnqueuePendingClaim", "EventsAfter",
			"FederationBindingByProject", "IssueByUID", "PatchProjectMetadata",
			"PendingFederationPushStats", "ProjectByID", "RemoveProject",
		},
		run: checkFederationProjectAdoption,
	},
	{
		name: "federation adoption after disconnect",
		methods: []string{
			"AckRelayDeliveries", "AdoptProjectIntoFederation", "ApplyUpstreamAttribution",
			"CreateIssue", "CreateProject", "ExportAttribution", "ExportRelayState",
			"LeaveFederationReplica", "PendingRelayDeliveries", "PinRootAuthority",
			"RotateRootAuthority", "SetRelayBindingConfig", "UpsertFederationBinding",
		},
		run: checkFederationAdoptionAfterDisconnect,
	},
	{
		name: "federation adoption project access",
		methods: []string{
			"AdoptProjectIntoFederation", "CreateProject", "CreateTeam", "ProjectAccessPolicy",
			"ProjectAccessRevision", "SetProjectAccessPolicy",
		},
		run: func(t *testing.T, store db.Storage) error {
			return checkAdoptionPreservesProjectAccessPolicy(t, store)
		},
	},
	{
		name: "federation enrollment project export",
		methods: []string{
			"CreateAPIToken", "CreateProject", "CreateRelayEnrollment", "ExportFederationEnrollments",
			"ImportReplay", "PinRootAuthority", "UpsertFederationBinding",
		},
		runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
			return checkProjectScopedRelayEnrollmentExportRoundTrip(t, store, backend)
		},
	},
	{
		name:    "empty federation attachment",
		methods: []string{"AdoptProjectIntoFederation", "CreateProjectAndEvent", "CreateIssue", "CreateRecurrence", "PatchProjectMetadata", "PendingFederationPushEvents", "EventsAfter", "ImportReplay"},
		run:     checkEmptyFederationAttachment,
	},
	{
		name: "external import lifecycle",
		methods: []string{
			"AddLabel", "ClaimIssueSyncBinding", "CommentsByIssue", "CreateLink", "CreateProject",
			"EventsAfter", "ImportBatch", "ImportMappingBySource", "IssueByID", "LabelsByIssue",
			"LinkByEndpoints", "LinkByID", "ListIssues", "UpsertFederationBinding",
			"UpsertIssueSyncBinding",
		},
		run: checkExternalImportLifecycle,
	},
	{
		name: "external import edge cases",
		methods: []string{
			"CommentsByIssue", "CreateLink", "CreateProject", "EditIssue", "ImportBatch",
			"ImportMappingBySource", "IssueByID", "LinkByEndpoints", "LinkByID",
			"UpsertFederationBinding",
		},
		run: checkExternalImportEdgeCases,
	},
	{
		name: "external import comment timestamp precision",
		methods: []string{
			"CommentsByIssue", "CreateProject", "EventsAfter", "ImportBatch",
			"ImportMappingBySource",
		},
		run: checkImportCommentTimestampPrecision,
	},
	{
		name: "external import assignment expiry",
		methods: []string{
			"ClaimOwner", "CreateProject", "EventsAfter", "ExpireAssignments",
			"ImportBatch", "ImportMappingBySource", "IssueByID",
		},
		run: checkExternalImportAssignmentExpiry,
	},
	{
		name: "snapshot replay core",
		methods: []string{
			"AddLabel", "AttachAlias", "CommentsByIssue", "CreateAPIToken", "CreateComment",
			"CreateIssue", "CreateLink", "CreateProject", "EventsByUIDs", "ExportComments",
			"ExportEvents", "ExportFederationBindings", "ExportFederationEnrollments",
			"ExportFederationQuarantine", "ExportFederationSyncStatus", "ExportImportMappings",
			"ExportIssueClaims", "ExportIssueLabels", "ExportIssueSyncBindings", "ExportIssueSyncStatus",
			"ExportIssues", "ExportLinks", "ExportMeta", "ExportPendingClaimRequests",
			"ExportProjectAliases", "ExportProjectPurgeLog", "ExportProjects", "ExportPurgeLog",
			"ExportRecurrences", "ExportSequences", "ImportReplay", "IssueByUID", "LabelsByIssue",
			"LinkByEndpoints", "ListAPITokens", "ProjectAliases", "ProjectByUID", "ResolveAPIToken",
			"RevokeAPIToken", "SchemaVersion", "SystemProject", "UpsertImportMapping",
		},
		runWithBackend: checkSnapshotReplayCore,
	},
	{
		name: "snapshot replay extended state",
		methods: []string{
			"ActiveFederationQuarantine", "CommentsByIssue", "ExportExternalFieldMappings",
			"ExportExternalFieldStates", "ExportExternalRootBindings", "ExportImportMappings",
			"ExportProjectPurgeLog", "ExportPurgeLog", "ExportSequences", "FederationBindingByProject",
			"FederationSyncStatusByProject", "GetRecurrenceByUID", "ImportReplay", "IssueByUID",
			"IssueSyncBindingByProject", "IssueSyncStatusByProject", "LabelsByIssue",
			"ListFederationEnrollments", "ListPendingClaimRequests", "ProjectAliases", "ProjectByUID",
		},
		run: checkSnapshotReplayExtendedState,
	},
	{
		name:    "snapshot replay rejects malformed retained status scan",
		methods: []string{"ImportReplay", "CreateProject", "ProjectByUID", "IssueSyncBindingByProject"},
		run:     checkSnapshotReplayRejectsMalformedRetainedStatusScan,
	},
	{
		name:    "snapshot replay rejects invalid external root frontiers",
		methods: []string{"ImportReplay", "ProjectByUID"},
		run:     checkSnapshotReplayRejectsInvalidExternalRootFrontiers,
	},
	{
		name:    "snapshot replay rejects invalid binding mappings",
		methods: []string{"ImportReplay", "ProjectByUID"},
		run:     checkSnapshotReplayRejectsInvalidBindingMappings,
	},
	{
		name:    "snapshot replay rejects duplicate field mapping identities",
		methods: []string{"ImportReplay", "ProjectByUID"},
		run:     checkSnapshotReplayRejectsDuplicateFieldMappingIdentities,
	},
	{
		name: "snapshot replay preserves submicrosecond field mapping identities",
		methods: []string{
			"ExternalFieldStates", "ExternalRootBindingByIssue", "ImportReplay", "IssueByUID",
			"ListExternalFieldMappings",
		},
		run: checkSnapshotReplayPreservesSubmicrosecondFieldMappingIdentities,
	},
	{
		name: "snapshot replay compatibility options",
		methods: []string{
			"EventsByUIDs", "ImportReplay", "IssueSyncBindingByProject",
			"ListPendingClaimRequests", "ProjectByUID",
		},
		run: checkSnapshotReplayCompatibilityOptions,
	},
	{
		name:    "snapshot replay preserves historical project names",
		methods: []string{"EventsAfter", "ImportReplay", "ProjectByUID"},
		run:     checkSnapshotReplayHistoricalProjectName,
	},
	{
		name:    "snapshot replay rejects unsafe historical project names",
		methods: []string{"ImportReplay", "ListProjects"},
		run:     checkSnapshotReplayUnsafeHistoricalProjectName,
	},
	{
		name: "snapshot replay rejects malformed event entries",
		methods: []string{
			"EventsByUIDs", "ImportReplay", "ProjectByUID",
		},
		run: checkSnapshotReplayRejectsMalformedEventEntries,
	},
	{
		name: "snapshot replay atomic rejection",
		methods: []string{
			"CreateProject", "ImportReplay", "ListProjects", "ProjectByUID", "SystemProject",
		},
		run: checkSnapshotReplayAtomicRejection,
	},
	{
		name: "snapshot replay project envelopes",
		methods: []string{
			"CreateIssue", "CreateLink", "CreateProject", "ExportComments", "ExportEvents",
			"ExportFederationBindings", "ExportFederationEnrollments", "ExportFederationQuarantine",
			"ExportFederationSyncStatus", "ExportImportMappings", "ExportIssueClaims", "ExportIssueLabels",
			"ExportIssueSyncBindings", "ExportIssueSyncStatus", "ExportIssues", "ExportLinks", "ExportMeta",
			"ExportPendingClaimRequests", "ExportProjectAliases", "ExportProjectPurgeLog", "ExportProjects",
			"ExportPurgeLog", "ExportRecurrences", "ExportSequences", "ImportMappingBySource",
			"ImportReplay", "LinkByEndpoints", "LinkByID", "UpsertImportMapping",
		},
		runWithBackend: checkSnapshotReplayProjectEnvelopes,
	},
	{
		name: "snapshot merge external roots",
		methods: []string{
			"CreateExternalRootBinding", "CreateIssue", "CreateProject", "ExportComments", "ExportEvents", "ExportExternalFieldMappings",
			"ExportExternalFieldStates", "ExportExternalRootBindings", "ExportFederationBindings",
			"ExportFederationEnrollments", "ExportFederationQuarantine", "ExportFederationSyncStatus",
			"ExportImportMappings", "ExportIssueClaims", "ExportIssueLabels", "ExportIssueSyncBindings",
			"ExportIssueSyncStatus", "ExportIssues", "ExportLinks", "ExportMeta", "ExportPendingClaimRequests",
			"ExportProjectAliases", "ExportProjectPurgeLog", "ExportProjects", "ExportPurgeLog",
			"ExportRecurrences", "ExportSequences", "ExternalFieldStates", "ExternalRootBindingByIssue",
			"ImportReplay", "IssueByUID", "ListExternalFieldMappings", "PauseExternalRootBinding", "ProjectByUID",
			"UpsertExternalFieldMapping", "UpsertImportMapping",
		},
		runWithBackend: checkSnapshotMergeExternalRoots,
	},
}

type issueFixture struct {
	Project db.Project
	Issue   db.Issue
}

// CoveredStorageMethods returns every Storage method exercised by shared
// scenarios expected to pass for the supplied backend. The list is checked
// against pgstore's mechanical implementation inventory so replacing a
// sentinel stub requires observable backend-neutral coverage in the same
// change.
func CoveredStorageMethods(expectedFailures map[string]error) map[string]struct{} {
	covered := map[string]struct{}{"Close": {}}
	for _, test := range storageScenarios {
		if _, expectedToFail := expectedFailures[test.name]; expectedToFail {
			continue
		}
		for _, method := range test.methods {
			covered[method] = struct{}{}
		}
	}
	return covered
}

// RunStorageConformance exercises the same observable behaviors against one
// backend. Known gaps remain visible as exact expected failures rather than
// being hidden behind interface satisfaction or schema-presence checks.
func RunStorageConformance(t *testing.T, backend Backend) {
	t.Helper()
	require.NotEmpty(t, backend.Name)
	require.NotNil(t, backend.Open)

	scenarioNames := make(map[string]struct{}, len(storageScenarios))
	for _, test := range storageScenarios {
		scenarioNames[test.name] = struct{}{}
	}
	for name, want := range backend.ExpectedFailures {
		_, ok := scenarioNames[name]
		assert.True(t, ok, "expected failure names unknown scenario %q", name)
		assert.Error(t, want, "expected failure %q must name an error", name)
	}

	for _, test := range storageScenarios {
		t.Run(test.name, func(t *testing.T) {
			store := backend.Open(t)
			require.NotNil(t, store)
			t.Cleanup(func() {
				require.NoError(t, store.Close())
			})

			var err error
			if test.runWithBackend != nil {
				err = test.runWithBackend(t, store, backend)
			} else {
				err = test.run(t, store)
			}
			want, expected := backend.ExpectedFailures[test.name]
			if expected {
				require.Error(t, err, "%s unexpectedly passed; remove its expected failure", test.name)
				assert.ErrorIs(t, err, want)
				return
			}
			require.NoError(t, err)
		})
	}
}

// RunExternalRootConformance exercises only the external-root durable-state
// scenario. It keeps the focused backend command fast while using the exact
// same checks as the complete storage conformance suite.
func RunExternalRootConformance(t *testing.T, backend Backend) {
	t.Helper()
	require.NotEmpty(t, backend.Name)
	require.NotNil(t, backend.Open)

	checks := []struct {
		name string
		run  func(*testing.T, db.Storage) error
	}{
		{name: "durable state", run: func(t *testing.T, store db.Storage) error {
			return checkExternalRootBindings(t, store, backend)
		}},
		{name: "safety invariants", run: func(t *testing.T, store db.Storage) error {
			return checkExternalRootSafetyInvariants(t, store, backend)
		}},
		{name: "content ownership", run: func(t *testing.T, store db.Storage) error {
			return checkExternalRootContentOwnership(t, store, backend)
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			store := backend.Open(t)
			require.NotNil(t, store)
			t.Cleanup(func() {
				require.NoError(t, store.Close())
			})
			require.NoError(t, check.run(t, store))
		})
	}
}

// RunExternalRootContentOwnershipConformance exercises the native mutation
// boundaries that must preserve externally owned title/body content.
func RunExternalRootContentOwnershipConformance(t *testing.T, backend Backend) {
	t.Helper()
	require.NotEmpty(t, backend.Name)
	require.NotNil(t, backend.Open)
	store := backend.Open(t)
	require.NotNil(t, store)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, checkExternalRootContentOwnership(t, store, backend))
}
