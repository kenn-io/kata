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
	InstallExternalRootClock            func(db.Storage, func() time.Time) func()
	InstallRelayHistoryFullScanObserver func(db.Storage, func()) func()
	SeedLegacyPendingClaim              func(context.Context, db.Storage, string) error
	SeedClaimViolation                  func(context.Context, db.Storage, db.Project, db.Issue, string, jsontext.Value) error
	SeedUnsupportedFederationEvent      func(context.Context, db.Storage, db.Project, string) error
	BackdateCommentCreated              func(context.Context, db.Storage, int64, time.Time) error
	InstallEnrollmentInsertFailure      func(context.Context, db.Storage) (func() error, error)
	InstallEnrollmentRotationStage      func(db.Storage, func(context.Context) error) func()
	ExpectedFailures                    map[string]error
}

type scenario struct {
	name           string
	methods        []string
	run            func(t *testing.T, store db.Storage) error
	runWithBackend func(t *testing.T, store db.Storage, backend Backend) error
}

var storageScenarios = []scenario{
	{name: "relay ingress", methods: []string{"AcceptRelayDeliveries", "CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "IssueByUID", "LinksByIssue", "MaterializeFederatedProject", "PinRootAuthority", "UpsertFederationBinding"}, run: func(t *testing.T, store db.Storage) error { RunRelayIngressAtomicity(t, store); return nil }},
	{name: "relay topology", methods: []string{"SetRelayBindingConfig"}, run: func(t *testing.T, store db.Storage) error { RunRelayTopology(t, store); return nil }},
	{name: "relay enrollment", methods: []string{"CreateRelayEnrollment"}, run: func(t *testing.T, store db.Storage) error { RunRelayEnrollmentScope(t, store); return nil }},
	{name: "relay enrollment history validation cache", methods: []string{"CreateAPIToken", "CreateIssue", "CreateProject", "CreateRelayEnrollment", "CreateComment", "ProjectAccessTransactionFence", "RemoveProject", "PurgeProject", "PinRootAuthority", "RevokeFederationEnrollment", "UpsertFederationBinding"}, runWithBackend: func(t *testing.T, store db.Storage, backend Backend) error {
		RunRelayEnrollmentHistoryValidationCache(t, store, backend)
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
	{name: "screen view claims", methods: []string{"ClaimScreenView", "ReleaseScreenView"}, run: checkScreenViewClaims},
	{name: "external import derived status", methods: []string{"CreateProject", "ImportBatch", "ImportMappingBySource", "IssueByID", "EditIssue"}, run: checkImportDerivedStatus},
	{name: "issue status federation intent", methods: []string{"IngestFederationEvents", "MaterializeFederatedProject", "CreateIssue", "UpsertIssueSyncBinding"}, run: checkIssueStatusFederationIntent},
	{name: "issue status native intent", methods: []string{"CloseIssueWithEvents", "ReopenIssue", "CreateIssue", "UpsertIssueSyncBinding"}, runWithBackend: checkIssueStatusNativeIntent},
	{name: "issue status mapping reads", methods: []string{"CreateProject", "CreateIssue", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding"}, run: checkIssueStatusMappingReads},
	{name: "issue status mapping storage", methods: []string{"CreateProject", "CreateIssue", "UpsertImportMapping"}, run: checkIssueStatusMappingStorage},
	{name: "issue status observation", methods: []string{"CreateProject", "CreateIssue", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding"}, runWithBackend: checkIssueStatusObservation},
	{name: "issue status content isolation", methods: []string{"CreateProject", "ImportBatch", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding"}, run: checkIssueStatusContentIsolation},
	{name: "issue status close metadata", methods: []string{"CreateProject", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding", "UpsertImportMapping"}, run: checkIssueStatusProviderCloseMetadata},
	{name: "issue status archive fence", methods: []string{"CreateProject", "ClaimIssueSyncBinding", "UpsertIssueSyncBinding", "RemoveProject", "RestoreProject"}, run: checkIssueStatusArchiveFence},
	{name: "issue status locator", methods: []string{"CreateIssue", "CreateProject", "CloseIssue", "ClaimIssueSyncBinding", "SoftDeleteIssue", "UpsertIssueSyncBinding", "UpsertImportMapping"}, runWithBackend: checkIssueStatusLocator},
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
