// Package db hosts the neutral Storage contract — interface, parameter
// structs, sentinel errors, and pure helpers — shared by every backend.
// Concrete implementations live in sibling packages (sqlitestore today;
// pgstore in Phase 3); production entry points pick a backend through the
// storeopen DSN dispatcher and hold a db.Storage thereafter.
package db

import (
	"context"
	"iter"
	"time"
)

// Storage is the backend-neutral domain API. The current implementation is
// *sqlitestore.Store; a future Postgres backend will satisfy the same interface.
// Transactions are an implementation detail and never appear here. Production
// entry points hold a db.Storage; backend selection happens through the
// storeopen DSN dispatcher.
type Storage interface {
	// identity / lifecycle
	InstanceUID() string
	RefreshInstanceUID(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	Path() string
	Close() error
	RetryTransient(ctx context.Context, op func() error) error
	// Migrate brings the backend's schema up to db.CurrentSchemaVersion() by
	// applying every pending migration file in order. Idempotent: returns
	// MigrationResult{From: N, To: N, Applied: nil} when already current. On
	// a backend opened read-only, returns an error.
	Migrate(ctx context.Context) (MigrationResult, error)

	// projects + aliases
	CreateProject(ctx context.Context, name string) (Project, error)
	CreateProjectWithUID(ctx context.Context, name, projectUID string) (Project, error)
	ProjectByID(ctx context.Context, id int64) (Project, error)
	ProjectByName(ctx context.Context, name string) (Project, error)
	ProjectByNameIncludingArchived(ctx context.Context, name string) (Project, error)
	ProjectByUID(ctx context.Context, uid string) (Project, error)
	ListProjects(ctx context.Context) ([]Project, error)
	ListProjectsIncludingArchived(ctx context.Context) ([]Project, error)
	RenameProject(ctx context.Context, id int64, name string) (Project, error)
	RemoveProject(ctx context.Context, p RemoveProjectParams) (Project, *Event, error)
	RestoreProject(ctx context.Context, projectID int64, actor string) (Project, *Event, bool, error)
	HardDeleteProject(ctx context.Context, id int64) error
	MergeProjects(ctx context.Context, p MergeProjectsParams) (ProjectMergeResult, error)
	MoveIssueProject(ctx context.Context, in MoveIssueProjectIn) (MoveIssueProjectOut, error)
	PatchProjectMetadata(ctx context.Context, in PatchProjectMetadataIn) (PatchProjectMetadataOut, error)
	BatchProjectStats(ctx context.Context) (map[int64]ProjectStats, error)
	AliasByID(ctx context.Context, id int64) (ProjectAlias, error)
	AliasByIdentity(ctx context.Context, identity string) (ProjectAlias, error)
	AttachAlias(ctx context.Context, projectID int64, identity, kind, rootPath string) (ProjectAlias, error)
	ReassignAlias(ctx context.Context, aliasID, projectID int64, rootPath string) error
	DetachProjectAlias(ctx context.Context, p DetachAliasParams) (ProjectAlias, *Event, error)
	TouchAlias(ctx context.Context, aliasID int64, rootPath string) error
	ProjectAliases(ctx context.Context, projectID int64) ([]ProjectAlias, error)
	LatestAliasForProject(ctx context.Context, projectID int64) (AliasRow, bool, error)

	// issues
	CreateIssue(ctx context.Context, p CreateIssueParams) (Issue, Event, error)
	IssueByID(ctx context.Context, id int64) (Issue, error)
	IssueByShortID(ctx context.Context, projectID int64, shortID string, include IncludeDeleted) (Issue, error)
	IssueByUID(ctx context.Context, issueUID string, include IncludeDeleted) (Issue, error)
	IssueUIDPrefixMatch(ctx context.Context, prefix string, limit int, include IncludeDeleted) ([]Issue, error)
	ListIssues(ctx context.Context, p ListIssuesParams) ([]Issue, error)
	ListAllIssues(ctx context.Context, p ListAllIssuesParams) ([]Issue, error)
	ReadyIssues(ctx context.Context, projectID int64, limit int, filter ReadyIssuesFilter) ([]Issue, error)
	ReadyIssuesGlobal(ctx context.Context, limit int) ([]ReadyGlobalIssue, error)
	ChildrenOfIssue(ctx context.Context, projectID, parentIssueID int64) ([]Issue, error)
	OpenChildrenOf(ctx context.Context, projectID, parentIssueID int64, limit int) ([]Issue, int, error)
	EditIssue(ctx context.Context, p EditIssueParams) (Issue, *Event, bool, error)
	EditIssueAtomic(ctx context.Context, p EditIssueAtomicParams) (EditIssueAtomicResult, error)
	CloseIssue(ctx context.Context, issueID int64, reason, actor, message string, evidence []Evidence) (Issue, *Event, bool, error)
	CloseIssueWithEvents(ctx context.Context, issueID int64, reason, actor, message string, evidence []Evidence) (Issue, []Event, bool, error)
	ReopenIssue(ctx context.Context, issueID int64, actor string) (Issue, *Event, bool, error)
	SoftDeleteIssue(ctx context.Context, issueID int64, actor string) (Issue, *Event, bool, error)
	RestoreIssue(ctx context.Context, issueID int64, actor string) (Issue, *Event, bool, error)
	PurgeIssue(ctx context.Context, issueID int64, actor string, reason *string) (PurgeLog, error)
	ClaimOwner(ctx context.Context, issueID int64, actor string, force bool) (ClaimResult, error)
	UpdateOwner(ctx context.Context, issueID int64, newOwner *string, actor string) (Issue, *Event, bool, error)
	UpdatePriority(ctx context.Context, issueID int64, newPriority *int64, actor string) (Issue, *Event, bool, error)
	PatchIssueMetadata(ctx context.Context, in PatchIssueMetadataIn) (PatchIssueMetadataOut, error)
	ShortIDsByUIDs(ctx context.Context, projectID int64, uids []string) (map[string]string, error)
	PurgeResetCheck(ctx context.Context, afterID, projectID int64) (int64, error)

	// comments
	CreateComment(ctx context.Context, p CreateCommentParams) (Comment, Event, error)
	CommentBodyByID(ctx context.Context, id int64) (string, error)
	CommentsByIssue(ctx context.Context, issueID int64) ([]Comment, error)

	// labels
	AddLabel(ctx context.Context, issueID int64, label, author string) (IssueLabel, error)
	AddLabelAndEvent(ctx context.Context, issueID int64, ev LabelEventParams) (IssueLabel, Event, error)
	RemoveLabel(ctx context.Context, issueID int64, label string) error
	RemoveLabelAndEvent(ctx context.Context, issueID int64, ev LabelEventParams) (Event, error)
	HasLabel(ctx context.Context, issueID int64, label string) (bool, error)
	LabelByEndpoints(ctx context.Context, issueID int64, label string) (IssueLabel, error)
	LabelCounts(ctx context.Context, projectID int64) ([]LabelCount, error)
	LabelsByIssue(ctx context.Context, issueID int64) ([]IssueLabel, error)
	LabelsByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]string, error)
	LabelsForIssue(ctx context.Context, issueID int64) ([]string, error)

	// links
	CreateLink(ctx context.Context, p CreateLinkParams) (Link, error)
	CreateLinkAndEvent(ctx context.Context, p CreateLinkParams, ev LinkEventParams) (Link, Event, error)
	DeleteLinkByID(ctx context.Context, linkID int64) error
	DeleteLinkAndEvent(ctx context.Context, link Link, ev LinkEventParams) (Event, error)
	LinkByID(ctx context.Context, id int64) (Link, error)
	LinkByEndpoints(ctx context.Context, fromIssueID, toIssueID int64, linkType string) (Link, error)
	LinksByIssue(ctx context.Context, issueID int64) ([]Link, error)
	ParentOf(ctx context.Context, childIssueID int64) (Link, error)
	ChildCountsByParents(ctx context.Context, projectID int64, parentIssueIDs []int64) (map[int64]ChildCounts, error)
	ParentNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64]int64, error)
	ParentShortIDsByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64]string, error)
	BlockNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]int64, error)
	BlockedByNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]int64, error)
	RelatedNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]int64, error)

	// recurrences
	CreateRecurrence(ctx context.Context, in CreateRecurrenceIn) (Recurrence, error)
	GetRecurrenceByID(ctx context.Context, id int64) (Recurrence, error)
	GetRecurrenceByUID(ctx context.Context, recUID string) (Recurrence, error)
	ListRecurrencesByProject(ctx context.Context, projectID int64) ([]Recurrence, error)
	PatchRecurrence(ctx context.Context, in PatchRecurrenceIn) (PatchRecurrenceOut, error)
	SoftDeleteRecurrence(ctx context.Context, id int64, actor string) error
	MaterializeNext(ctx context.Context, recurrenceID int64, afterKey, actor string) (MaterializeNextOut, error)

	// events / idempotency / close-throttle
	EventsAfter(ctx context.Context, p EventsAfterParams) ([]Event, error)
	EventsByUIDs(ctx context.Context, projectID int64, uids []string) ([]Event, error)
	EventsInWindow(ctx context.Context, p EventsInWindowParams) ([]Event, error)
	MaxEventID(ctx context.Context) (int64, error)
	MaxLocalOriginEventID(ctx context.Context, projectID int64) (int64, error)
	MaxFederationBaselineEventID(ctx context.Context, projectID, sinceEventID int64) (int64, error)
	LookupIdempotency(ctx context.Context, projectID int64, key string, since time.Time) (*IdempotencyMatch, error)
	InsertCloseThrottledEvent(ctx context.Context, issueID int64, actor string, payload CloseThrottledPayload) (Event, error)
	RecentSiblingCloses(ctx context.Context, projectID, parentIssueID, excludeIssueID int64, actor string, since time.Time) ([]Event, error)
	RecentSameMessageClose(ctx context.Context, projectID, parentIssueID, excludeIssueID int64, actor, normalizedMessage string, since time.Time) (*Event, error)

	// search
	SearchFTS(ctx context.Context, projectID int64, q string, limit int, includeDeleted bool) ([]SearchCandidate, error)
	SearchFTSAny(ctx context.Context, projectID int64, q string, limit int, includeDeleted bool) ([]SearchCandidate, error)

	// import support
	ImportBatch(ctx context.Context, p ImportBatchParams) (ImportBatchResult, []Event, error)
	UpsertImportMapping(ctx context.Context, p ImportMappingParams) (ImportMapping, error)
	ImportMappingBySource(ctx context.Context, projectID int64, source, objectType, externalID string) (ImportMapping, error)
	ImportMappingsByProjectSource(ctx context.Context, projectID int64, source string) ([]ImportMapping, error)
	ImportReplay(ctx context.Context, recs []ImportRecord, opts ImportOptions) error

	// API tokens / system project
	EnsureSystemProject(ctx context.Context) error
	SystemProject(ctx context.Context) (Project, error)
	CreateAPIToken(ctx context.Context, p CreateAPITokenParams) (APIToken, Event, error)
	RevokeAPIToken(ctx context.Context, id int64, adminActor string) (APIToken, Event, error)
	ResolveAPIToken(ctx context.Context, plaintext string) (APIToken, error)
	ListAPITokens(ctx context.Context) ([]APIToken, error)

	// claims
	AcquireClaim(ctx context.Context, p AcquireClaimParams) (LeaseResult, error)
	RenewClaim(ctx context.Context, p RenewClaimParams) (LeaseResult, error)
	ReleaseClaim(ctx context.Context, p ReleaseClaimParams) (LeaseResult, error)
	ForceReleaseClaim(ctx context.Context, p ForceReleaseClaimParams) (LeaseResult, error)
	ClaimStatus(ctx context.Context, projectID int64, issueRef string, now time.Time) (ClaimStatus, error)
	ClaimStatusReadOnly(ctx context.Context, projectID int64, issueRef string, now time.Time) (ClaimStatus, error)
	EnqueuePendingClaim(ctx context.Context, p PendingClaimParams) (PendingClaimRequest, error)
	ResolvePendingClaim(ctx context.Context, requestUID string, claim IssueClaim) error
	RejectPendingClaim(ctx context.Context, requestUID, reason string, now time.Time) error
	ListPendingClaimRequests(ctx context.Context, projectID int64, limit int) ([]PendingClaimRequest, error)
	ListPendingClaimRequestsForIssue(ctx context.Context, projectID int64, issueUID string, limit int) ([]PendingClaimRequest, error)
	CountLiveClaims(ctx context.Context, projectID int64) (int64, error)
	CountPendingClaims(ctx context.Context, projectID int64) (int64, error)
	MarkPendingClaimAttempt(ctx context.Context, requestUID, lastError string, now time.Time) error
	ClaimStatusRefreshError(ctx context.Context, projectID int64, issueUID string) (ClaimStatusRefreshError, error)
	MarkClaimStatusRefreshError(ctx context.Context, projectID int64, issueUID string, statusCode int, lastError string, now time.Time) error
	ClearClaimStatusRefreshError(ctx context.Context, projectID int64, issueUID string) error
	UpsertClaimCache(ctx context.Context, claim IssueClaim) error
	ApplyClaimStatus(ctx context.Context, projectID int64, issueUID string, status ClaimStatus) error
	CheckClaimGate(ctx context.Context, p ClaimGateParams) error
	ExpireTimedClaims(ctx context.Context, now time.Time, limit int) ([]Event, error)
	ExpireTimedClaimsForProject(ctx context.Context, projectID int64, now time.Time, limit int) ([]Event, error)
	UnresolvedClaimViolationsForIssue(ctx context.Context, projectID int64, issueUID string, limit int) ([]ClaimViolationSummary, int64, error)
	UnresolvedClaimViolationsForProject(ctx context.Context, projectID int64, limit int) ([]ClaimViolationSummary, int64, error)

	// federation: bindings + sync status + quarantines
	ListFederationBindings(ctx context.Context) ([]FederationBinding, error)
	FederationBindingByProject(ctx context.Context, projectID int64) (FederationBinding, error)
	FederationSyncStatusByProject(ctx context.Context, projectID int64) (FederationSyncStatus, error)
	RecordFederationSyncPullStarted(ctx context.Context, projectID int64, at time.Time) error
	RecordFederationSyncPullSuccess(ctx context.Context, projectID int64, at time.Time) error
	RecordFederationSyncPushStarted(ctx context.Context, projectID int64, at time.Time) error
	RecordFederationSyncPushSuccess(ctx context.Context, projectID int64, at time.Time) error
	RecordFederationSyncReset(ctx context.Context, projectID int64, at time.Time) error
	RecordFederationSyncError(ctx context.Context, projectID int64, syncErr error, at time.Time) error
	ClearFederationSyncError(ctx context.Context, projectID int64) error
	RecordFederationQuarantine(ctx context.Context, p RecordFederationQuarantineParams) (FederationQuarantine, error)
	ActiveFederationQuarantine(ctx context.Context, projectID int64, direction FederationQuarantineDirection) (FederationQuarantine, error)
	ActiveFederationQuarantinesByProject(ctx context.Context, projectID int64) ([]FederationQuarantine, error)
	CountActiveFederationEnrollments(ctx context.Context, projectID int64) (int64, error)
	SkipFederationQuarantine(ctx context.Context, p SkipFederationQuarantineParams) (FederationQuarantine, error)
	UpsertFederationBinding(ctx context.Context, b FederationBinding) (FederationBinding, error)
	AdoptProjectIntoFederation(ctx context.Context, p AdoptProjectIntoFederationParams) (AdoptProjectIntoFederationResult, error)
	AdvanceFederationPullCursor(ctx context.Context, projectID, nextCursor int64) error
	InsertRemoteEvent(ctx context.Context, projectID int64, ev RemoteEvent) (bool, error)
	EnableProjectFederation(ctx context.Context, projectID int64, actor string) (FederationBinding, error)
	RefreshProjectFederationBaseline(ctx context.Context, projectID int64, actor string) (FederationBinding, bool, error)
	MaterializeFederatedProject(ctx context.Context, projectID int64) error
	ResetFederatedProject(ctx context.Context, projectID, replayHorizonEventID, pullCursorEventID int64) error

	// federation: enrollments
	CreateFederationEnrollment(ctx context.Context, p CreateFederationEnrollmentParams) (CreatedFederationEnrollment, error)
	ListFederationEnrollments(ctx context.Context) ([]FederationEnrollment, error)
	RevokeFederationEnrollment(ctx context.Context, id int64) error
	AuthorizeFederationToken(ctx context.Context, token string, projectID int64, capability string) (FederationEnrollment, error)

	// export (JSONL)
	ExportMeta(ctx context.Context) iter.Seq2[MetaKV, error]
	ExportProjects(ctx context.Context, f ExportFilter) iter.Seq2[ProjectExport, error]
	ExportProjectAliases(ctx context.Context, f ExportFilter) iter.Seq2[AliasExport, error]
	ExportRecurrences(ctx context.Context, f ExportFilter) iter.Seq2[RecurrenceExport, error]
	ExportIssues(ctx context.Context, f ExportFilter) iter.Seq2[IssueExport, error]
	ExportComments(ctx context.Context, f ExportFilter) iter.Seq2[CommentExport, error]
	ExportIssueLabels(ctx context.Context, f ExportFilter) iter.Seq2[IssueLabelExport, error]
	ExportLinks(ctx context.Context, f ExportFilter) iter.Seq2[LinkExport, error]
	ExportImportMappings(ctx context.Context, f ExportFilter) iter.Seq2[ImportMappingExport, error]
	ExportFederationBindings(ctx context.Context, f ExportFilter) iter.Seq2[FederationBindingExport, error]
	ExportFederationSyncStatus(ctx context.Context, f ExportFilter) iter.Seq2[FederationSyncStatusExport, error]
	ExportFederationQuarantine(ctx context.Context, f ExportFilter) iter.Seq2[FederationQuarantineExport, error]
	ExportFederationEnrollments(ctx context.Context, f ExportFilter) iter.Seq2[FederationEnrollmentExport, error]
	ExportIssueClaims(ctx context.Context, f ExportFilter) iter.Seq2[IssueClaimExport, error]
	ExportPendingClaimRequests(ctx context.Context, f ExportFilter) iter.Seq2[PendingClaimRequestExport, error]
	ExportEvents(ctx context.Context, f ExportFilter) iter.Seq2[EventExport, error]
	ExportPurgeLog(ctx context.Context, f ExportFilter) iter.Seq2[PurgeLogExport, error]
	ExportSequences(ctx context.Context) iter.Seq2[SequenceExport, error]

	// federation: push + ingest
	PendingFederationPushEvents(ctx context.Context, projectID int64, originInstanceUID string, afterID int64, limit int) ([]Event, error)
	PendingFederationPushStats(ctx context.Context, projectID int64, originInstanceUID string, afterID int64) (int64, int64, error)
	AdvanceFederationPushCursor(ctx context.Context, projectID, nextCursor int64) error
	EnableFederationPush(ctx context.Context, projectID int64, cursor int64) (FederationBinding, error)
	ResetFederatedProjectIfNoPendingPush(ctx context.Context, projectID, replayHorizonEventID, pullCursorEventID int64, originInstanceUID string, pushCursorEventID int64) error
	IngestFederationEvents(ctx context.Context, p FederationIngestParams) (FederationIngestResult, error)
}
