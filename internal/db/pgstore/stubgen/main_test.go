package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestGenerateOmitsUnusedStandardLibraryImports(t *testing.T) {
	storagePath := filepath.Join(t.TempDir(), "storage.go")
	require.NoError(t, os.WriteFile(storagePath, []byte(`package db
import "context"
type Storage interface { Only(context.Context) error }
`), 0o600))

	generated, err := Generate(storagePath)
	require.NoError(t, err)
	file, err := parser.ParseFile(token.NewFileSet(), "stubs_gen.go", generated, parser.ImportsOnly)
	require.NoError(t, err)
	var imports []string
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)
		imports = append(imports, path)
	}
	assert.Equal(t, []string{"context", "go.kenn.io/kata/internal/db"}, imports)
}

func TestStorageMethodInventoryClassifiesEveryMethod(t *testing.T) {
	methods, err := CollectStorageMethodInventory("../../storage.go")
	require.NoError(t, err)
	require.Len(t, methods, 249)

	var implemented []string
	var stubbed []string
	for _, method := range methods {
		switch method.Status {
		case MethodImplemented:
			implemented = append(implemented, method.Name)
		case MethodStubbed:
			stubbed = append(stubbed, method.Name)
		default:
			assert.Fail(t, "unknown method status", "%s has status %q", method.Name, method.Status)
		}
	}

	sort.Strings(implemented)
	assert.Equal(t, []string{
		"AcquireClaim",
		"AcquireIdempotencyLock",
		"ActiveFederationQuarantine",
		"ActiveFederationQuarantinesByProject",
		"AddLabel",
		"AddLabelAndEvent",
		"AdoptProjectIntoFederation",
		"AdvanceFederationPullCursor",
		"AdvanceFederationPushCursor",
		"AliasByID",
		"AliasByIdentity",
		"ApplyClaimStatus",
		"ApplyExternalFieldProjection",
		"ApplyExternalRootProjection",
		"AttachAlias",
		"AuthorizeFederationToken",
		"BatchProjectStats",
		"CheckClaimGate",
		"ChildrenOfIssue",
		"ClaimExternalRootBinding",
		"ClaimExternalRootBindingForManualAction",
		"ClaimExternalRootBindingForManualReconcile",
		"ClaimIssueSyncBinding",
		"ClaimOwner",
		"ClaimStatus",
		"ClaimStatusReadOnly",
		"ClaimStatusRefreshError",
		"ClearClaimStatusRefreshError",
		"ClearFederationSyncError",
		"ClearPendingExternalComment",
		"Close",
		"CloseIssue",
		"CloseIssueGuarded",
		"CloseIssueWithEvents",
		"CommentBodyByID",
		"CommentsByIssue",
		"CountActiveFederationEnrollments",
		"CountLiveClaims",
		"CountOpenIssues",
		"CountPendingClaims",
		"CreateAPIToken",
		"CreateComment",
		"CreateExternalRootBinding",
		"CreateFederationEnrollment",
		"CreateIssue",
		"CreateLink",
		"CreateLinkAndEvent",
		"CreateProject",
		"CreateProjectAndEvent",
		"CreateProjectFederationEnrollment",
		"CreateProjectWithUID",
		"CreateProjectWithUIDAndEvent",
		"CreateRecurrence",
		"CreateRecurrenceForIssue",
		"DeleteLinkAndEvent",
		"DeleteLinkByID",
		"DesignateInboxProject",
		"DetachProjectAlias",
		"DisableIssueSyncBinding",
		"EditComment",
		"EditIssue",
		"EditIssueAtomic",
		"EnableFederationPush",
		"EnableProjectFederation",
		"EnqueuePendingClaim",
		"EnsureExternalRootLifecycleRequest",
		"EnsureSystemProject",
		"EventsAfter",
		"EventsByUIDs",
		"EventsInWindow",
		"ExpireTimedClaims",
		"ExpireTimedClaimsForProject",
		"ExportComments",
		"ExportEvents",
		"ExportExternalFieldMappings",
		"ExportExternalFieldStates",
		"ExportExternalRootBindings",
		"ExportFederationBindings",
		"ExportFederationEnrollments",
		"ExportFederationQuarantine",
		"ExportFederationSyncStatus",
		"ExportImportMappings",
		"ExportIssueClaims",
		"ExportIssueLabels",
		"ExportIssueSyncBindings",
		"ExportIssueSyncStatus",
		"ExportIssues",
		"ExportLinks",
		"ExportMeta",
		"ExportPendingClaimRequests",
		"ExportProjectAliases",
		"ExportProjectPurgeLog",
		"ExportProjects",
		"ExportPurgeLog",
		"ExportRecurrences",
		"ExportSequences",
		"ExternalFieldStates",
		"ExternalRootBindingByExternalKey",
		"ExternalRootBindingByID",
		"ExternalRootBindingByIssue",
		"FederationBindingByProject",
		"FederationEnrollmentTransactionFence",
		"FederationSyncStatusByProject",
		"FindActiveFederationEnrollment",
		"ForceReleaseClaim",
		"GetRecurrenceByID",
		"GetRecurrenceByUID",
		"HardDeleteProject",
		"HasLabel",
		"ImportBatch",
		"ImportCommentMappingsByIssue",
		"ImportMappingBySource",
		"ImportMappingsByProjectSource",
		"ImportReplay",
		"IngestFederationEvents",
		"InsertCloseThrottledEvent",
		"InsertRemoteEvent",
		"InstanceUID",
		"IssueByID",
		"IssueByShortID",
		"IssueByUID",
		"IssueQualifiersByUIDs",
		"IssueSyncBindingByID",
		"IssueSyncBindingByProject",
		"IssueSyncStatusByProject",
		"IssueUIDPrefixMatch",
		"LabelByEndpoints",
		"LabelCounts",
		"LabelsByIssue",
		"LabelsByIssues",
		"LabelsForIssue",
		"LatestAliasForProject",
		"LeaveFederationReplica",
		"LinkByEndpoints",
		"LinkByID",
		"LinksByIssue",
		"ListAPITokens",
		"ListAllIssues",
		"ListDueExternalRootBindings",
		"ListDueIssueSyncBindings",
		"ListExternalFieldMappings",
		"ListFederationBindings",
		"ListFederationEnrollments",
		"ListIssueContent",
		"ListIssues",
		"ListPendingClaimRequests",
		"ListPendingClaimRequestsForIssue",
		"ListProjectFederationEnrollments",
		"ListProjects",
		"ListProjectsIncludingArchived",
		"ListRecurrencesByProject",
		"LookupCommentIdempotency",
		"LookupIdempotency",
		"LookupIssueMutationIdempotency",
		"MarkClaimStatusRefreshError",
		"MarkPendingClaimAttempt",
		"MaterializeFederatedProject",
		"MaterializeNext",
		"MaxEventID",
		"MaxFederationBaselineEventID",
		"MaxLocalOriginEventID",
		"MergeProjects",
		"MoveIssueProject",
		"OpenChildrenOf",
		"ParentOf",
		"ParentShortIDsByIssues",
		"PatchIssueMetadata",
		"PatchProjectMetadata",
		"PatchRecurrence",
		"Path",
		"PauseExternalRootBinding",
		"PendingFederationPushEvents",
		"PendingFederationPushStats",
		"ProjectAliases",
		"ProjectByID",
		"ProjectByName",
		"ProjectByNameIncludingArchived",
		"ProjectByUID",
		"PurgeIssue",
		"PurgeProject",
		"PurgeResetCheck",
		"ReadyIssues",
		"ReadyIssuesGlobal",
		"ReassignAlias",
		"RebindFederationBinding",
		"RecentSameMessageClose",
		"RecentSiblingCloses",
		"ReconcileLocalFederationEcho",
		"RecordExternalRootError",
		"RecordExternalRootSuccess",
		"RecordFederationQuarantine",
		"RecordFederationSyncError",
		"RecordFederationSyncPullStarted",
		"RecordFederationSyncPullSuccess",
		"RecordFederationSyncPushStarted",
		"RecordFederationSyncPushSuccess",
		"RecordFederationSyncReset",
		"RecordIssueSyncError",
		"RecordIssueSyncSuccess",
		"RefreshInstanceUID",
		"RefreshIssueSyncBinding",
		"RefreshProjectFederationBaseline",
		"RejectPendingClaim",
		"RelationshipsByIssues",
		"ReleaseClaim",
		"ReleaseExternalRootClaim",
		"RemoveLabel",
		"RemoveLabelAndEvent",
		"RemoveProject",
		"RenameProject",
		"RenameProjectAndEvent",
		"RenewClaim",
		"RenewExternalRootClaim",
		"ReopenIssue",
		"ResetFederatedProject",
		"ResetFederatedProjectIfNoPendingPush",
		"ResolveAPIToken",
		"ResolveExternalFieldConflict",
		"ResolvePendingClaim",
		"RestoreIssue",
		"RestoreProject",
		"ResumeExternalRootBinding",
		"RetryFederationQuarantine",
		"RetryTransient",
		"RevokeAPIToken",
		"RevokeFederationEnrollment",
		"RewriteAuthorIdentity",
		"RotateFederationEnrollment",
		"SchemaVersion",
		"SearchFTS",
		"SearchFTSAny",
		"SetPendingExternalComment",
		"SkipFederationQuarantine",
		"SoftDeleteIssue",
		"SoftDeleteRecurrence",
		"SystemProject",
		"UnbindExternalRootBinding",
		"UnmapExternalField",
		"UnresolvedClaimViolationsForIssue",
		"UnresolvedClaimViolationsForProject",
		"UpdateOwner",
		"UpdatePriority",
		"UpsertClaimCache",
		"UpsertExternalCommentProjection",
		"UpsertExternalFieldMapping",
		"UpsertExternalFieldState",
		"UpsertFederationBinding",
		"UpsertImportMapping",
		"UpsertIssueSyncBinding",
	}, implemented)
	assert.Empty(t, stubbed)
}

func TestImplementedStorageMethodsHaveConformanceCoverage(t *testing.T) {
	methods, err := CollectStorageMethodInventory("../../storage.go")
	require.NoError(t, err)

	covered := dbtest.CoveredStorageMethods(pgstore.ExpectedConformanceFailures())
	declared := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		declared[method.Name] = struct{}{}
		if method.Status == MethodImplemented {
			assert.Contains(t, covered, method.Name,
				"implemented method %s needs a shared conformance scenario", method.Name)
		}
	}
	for method := range covered {
		assert.Contains(t, declared, method,
			"conformance coverage names unknown Storage method %s", method)
	}
}

// TestGeneratedStubsMatchSource regenerates stubs_gen.go in memory and
// compares against the committed file. A drift means someone touched
// internal/db/storage.go (added a method, changed a signature) without
// re-running `go generate`, or hand-edited stubs_gen.go directly.
//
// The committed stubs_gen.go is the source of truth at build time; this
// test guards against a stale-but-still-compiling state where the
// interface has grown a method we never stubbed.
func TestGeneratedStubsMatchSource(t *testing.T) {
	got, err := Generate("../../storage.go")
	require.NoError(t, err)

	want, err := os.ReadFile("../stubs_gen.go")
	require.NoError(t, err)

	assert.Equal(t, string(want), string(got),
		"stubs_gen.go is out of date — run `go generate ./internal/db/pgstore`")
}
