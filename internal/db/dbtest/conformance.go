// Package dbtest provides backend-neutral behavioral checks for db.Storage
// implementations. Backend packages supply only a fresh-store factory and an
// explicit list of temporary parity gaps.
package dbtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Backend configures one conformance run. Open must return a fresh store for
// every subtest. ExpectedFailures is an exact xfail manifest: a listed
// scenario must fail with the configured error, and unexpectedly passing it
// fails the suite until the stale entry is removed.
type Backend struct {
	Name                           string
	Open                           func(t *testing.T) db.Storage
	SeedLegacyPendingClaim         func(context.Context, db.Storage, string) error
	SeedClaimViolation             func(context.Context, db.Storage, db.Project, db.Issue, string, json.RawMessage) error
	SeedUnsupportedFederationEvent func(context.Context, db.Storage, db.Project, string) error
	ExpectedFailures               map[string]error
}

type scenario struct {
	name           string
	methods        []string
	run            func(t *testing.T, store db.Storage) error
	runWithBackend func(t *testing.T, store db.Storage, backend Backend) error
}

var storageScenarios = []scenario{
	{
		name:    "lifecycle",
		methods: []string{"InstanceUID", "Path", "RefreshInstanceUID", "RetryTransient", "SchemaVersion"},
		run:     checkLifecycle,
	},
	{
		name: "projects",
		methods: []string{
			"CreateProject", "CreateProjectWithUID", "ListProjects", "ListProjectsIncludingArchived",
			"ProjectByID", "ProjectByName", "ProjectByNameIncludingArchived", "ProjectByUID", "RenameProject",
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
		methods: []string{"AcquireIdempotencyLock", "CreateIssue", "CreateProject", "LookupIdempotency"},
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
			"BatchProjectStats", "ClaimOwner", "CloseIssue", "CloseIssueWithEvents", "CreateIssue", "CreateProject", "EditIssue",
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
			"ActivelyBlockedIssueIDs", "BlockNumbersByIssues", "BlockedByNumbersByIssues",
			"ChildCountsByParents", "ChildrenOfIssue", "CloseIssue", "CreateIssue", "CreateLink",
			"CreateLinkAndEvent", "CreateProject", "DeleteLinkAndEvent", "DeleteLinkByID",
			"LinkByEndpoints", "LinkByID", "LinksByIssue", "OpenChildrenOf", "ParentNumbersByIssues",
			"ParentOf", "ParentShortIDsByIssues", "RelatedNumbersByIssues",
		},
		run: checkLinksAndRelationshipProjections,
	},
	{
		name: "ready queues and discovery",
		methods: []string{
			"AddLabel", "CloseIssue", "CreateComment", "CreateIssue", "CreateLink", "CreateProject", "EditIssue",
			"IssueQualifiersByUIDs", "ListIssueContent", "ReadyIssues", "ReadyIssuesGlobal", "SearchFTS",
			"SearchFTSAny", "SoftDeleteIssue",
		},
		run: checkReadyQueuesAndDiscovery,
	},
	{
		name: "api tokens and system project",
		methods: []string{
			"CreateAPIToken", "EnsureSystemProject", "ListAPITokens", "ListProjects", "ProjectByID",
			"ResolveAPIToken", "RevokeAPIToken", "SystemProject",
		},
		run: checkAPITokensAndSystemProject,
	},
	{
		name: "recurrences",
		methods: []string{
			"CloseIssueWithEvents", "CreateProject", "CreateRecurrence", "EventsAfter", "GetRecurrenceByID",
			"GetRecurrenceByUID", "IssueByID", "LabelsForIssue", "ListIssues", "ListRecurrencesByProject",
			"MaterializeNext", "PatchRecurrence", "SoftDeleteRecurrence",
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
		name: "issue sync lifecycle",
		methods: []string{
			"ClaimIssueSyncBinding", "CreateProject", "DisableIssueSyncBinding", "IssueSyncBindingByID",
			"IssueSyncBindingByProject", "IssueSyncStatusByProject", "ListDueIssueSyncBindings",
			"RecordIssueSyncError", "RecordIssueSyncSuccess", "RefreshIssueSyncBinding", "UpsertIssueSyncBinding",
		},
		run: checkIssueSyncLifecycle,
	},
	{
		name: "metadata and atomic edit",
		methods: []string{
			"CreateIssue", "CreateProject", "EditIssueAtomic", "EventsAfter", "IssueByID", "LinkByEndpoints",
			"ListIssueContent", "PatchIssueMetadata", "PatchProjectMetadata", "ProjectByID",
		},
		run: checkMetadataAndAtomicEdit,
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
		name: "project relocation",
		methods: []string{
			"AcquireClaim", "ClaimStatusReadOnly", "CountLiveClaims", "CountPendingClaims", "CreateIssue",
			"CreateLink", "CreateProject", "CreateRecurrence", "EnqueuePendingClaim", "EventsAfter",
			"ImportMappingBySource", "IssueByID", "LinkByEndpoints", "ListPendingClaimRequestsForIssue",
			"MaterializeNext", "MoveIssueProject", "UpsertImportMapping",
		},
		run: checkProjectRelocation,
	},
	{
		name: "project merge",
		methods: []string{
			"AttachAlias", "CreateIssue", "CreateLink", "CreateProject", "CreateRecurrence", "EventsAfter",
			"ImportMappingBySource", "IssueByShortID", "LinksByIssue", "MergeProjects", "ProjectAliases",
			"ListRecurrencesByProject", "ProjectByID", "PurgeIssue", "RemoveProject", "SystemProject",
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
			"CreateIssue", "CreateLink", "CreateProject", "EnableFederationPush", "ExportFederationBindings",
			"ExportFederationEnrollments", "ExportFederationQuarantine", "ExportFederationSyncStatus",
			"FederationBindingByProject", "FederationSyncStatusByProject", "ListFederationBindings",
			"ListFederationEnrollments", "LinkByEndpoints", "PurgeIssue", "RecordFederationQuarantine", "RecordFederationSyncError",
			"RecordFederationSyncPullStarted", "RecordFederationSyncPullSuccess",
			"RecordFederationSyncPushStarted", "RecordFederationSyncPushSuccess",
			"RecordFederationSyncReset", "RetryFederationQuarantine", "RevokeFederationEnrollment",
			"SkipFederationQuarantine", "UpsertFederationBinding",
		},
		run: checkFederationControlLifecycle,
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
			"AcquireClaim", "CountLiveClaims", "CountPendingClaims", "CreateIssue", "CreateProject",
			"EnqueuePendingClaim", "EventsAfter", "FederationBindingByProject", "IssueByUID",
			"RecordFederationQuarantine", "ResetFederatedProject", "ResetFederatedProjectIfNoPendingPush",
			"UpsertFederationBinding",
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
		name: "federation project adoption",
		methods: []string{
			"AcquireClaim", "AdoptProjectIntoFederation", "CountLiveClaims", "CountPendingClaims",
			"CreateComment", "CreateIssue", "CreateProject", "EnqueuePendingClaim", "EventsAfter",
			"FederationBindingByProject", "IssueByUID", "PatchProjectMetadata",
			"PendingFederationPushStats", "ProjectByID", "RemoveProject",
		},
		run: checkFederationProjectAdoption,
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
			"ActiveFederationQuarantine", "CommentsByIssue", "ExportImportMappings",
			"ExportProjectPurgeLog", "ExportPurgeLog", "ExportSequences", "FederationBindingByProject",
			"FederationSyncStatusByProject", "GetRecurrenceByUID", "ImportReplay", "IssueByUID",
			"IssueSyncBindingByProject", "IssueSyncStatusByProject", "LabelsByIssue",
			"ListFederationEnrollments", "ListPendingClaimRequests", "ProjectAliases", "ProjectByUID",
		},
		run: checkSnapshotReplayExtendedState,
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
