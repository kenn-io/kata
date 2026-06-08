package daemon

import (
	"context"
	"time"

	"go.kenn.io/kata/internal/db"
)

var _ db.Storage = (*retryingStorage)(nil)

type retryingStorage struct {
	db.Storage
}

func retryDaemonWrites(store db.Storage) db.Storage {
	if _, ok := store.(*retryingStorage); ok {
		return store
	}
	return &retryingStorage{Storage: store}
}

func retryWrite0(ctx context.Context, store db.Storage, op func() error) error {
	return store.RetryTransient(ctx, op)
}

func retryWrite1[T any](ctx context.Context, store db.Storage, op func() (T, error)) (T, error) {
	var out T
	err := store.RetryTransient(ctx, func() error {
		var err error
		out, err = op()
		return err
	})
	return out, err
}

func retryWrite2[A, B any](ctx context.Context, store db.Storage, op func() (A, B, error)) (A, B, error) {
	var a A
	var b B
	err := store.RetryTransient(ctx, func() error {
		var err error
		a, b, err = op()
		return err
	})
	return a, b, err
}

func retryWrite3[A, B, C any](ctx context.Context, store db.Storage, op func() (A, B, C, error)) (A, B, C, error) {
	var a A
	var b B
	var c C
	err := store.RetryTransient(ctx, func() error {
		var err error
		a, b, c, err = op()
		return err
	})
	return a, b, c, err
}

func (s *retryingStorage) RefreshInstanceUID(ctx context.Context) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RefreshInstanceUID(ctx)
	})
}

func (s *retryingStorage) CreateProject(ctx context.Context, name string) (db.Project, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Project, error) {
		return s.Storage.CreateProject(ctx, name)
	})
}

func (s *retryingStorage) CreateProjectWithUID(ctx context.Context, name, projectUID string) (db.Project, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Project, error) {
		return s.Storage.CreateProjectWithUID(ctx, name, projectUID)
	})
}

func (s *retryingStorage) RenameProject(ctx context.Context, id int64, name string) (db.Project, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Project, error) {
		return s.Storage.RenameProject(ctx, id, name)
	})
}

func (s *retryingStorage) RemoveProject(ctx context.Context, p db.RemoveProjectParams) (db.Project, *db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.Project, *db.Event, error) {
		return s.Storage.RemoveProject(ctx, p)
	})
}

func (s *retryingStorage) RestoreProject(ctx context.Context, projectID int64, actor string) (db.Project, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Project, *db.Event, bool, error) {
		return s.Storage.RestoreProject(ctx, projectID, actor)
	})
}

func (s *retryingStorage) HardDeleteProject(ctx context.Context, id int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.HardDeleteProject(ctx, id)
	})
}

func (s *retryingStorage) MergeProjects(ctx context.Context, p db.MergeProjectsParams) (db.ProjectMergeResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.ProjectMergeResult, error) {
		return s.Storage.MergeProjects(ctx, p)
	})
}

func (s *retryingStorage) MoveIssueProject(ctx context.Context, in db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	return retryWrite1(ctx, s.Storage, func() (db.MoveIssueProjectOut, error) {
		return s.Storage.MoveIssueProject(ctx, in)
	})
}

func (s *retryingStorage) PatchProjectMetadata(ctx context.Context, in db.PatchProjectMetadataIn) (db.PatchProjectMetadataOut, error) {
	return retryWrite1(ctx, s.Storage, func() (db.PatchProjectMetadataOut, error) {
		return s.Storage.PatchProjectMetadata(ctx, in)
	})
}

func (s *retryingStorage) AttachAlias(ctx context.Context, projectID int64, identity, kind string) (db.ProjectAlias, error) {
	return retryWrite1(ctx, s.Storage, func() (db.ProjectAlias, error) {
		return s.Storage.AttachAlias(ctx, projectID, identity, kind)
	})
}

func (s *retryingStorage) ReassignAlias(ctx context.Context, aliasID, projectID int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ReassignAlias(ctx, aliasID, projectID)
	})
}

func (s *retryingStorage) DetachProjectAlias(ctx context.Context, p db.DetachAliasParams) (db.ProjectAlias, *db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.ProjectAlias, *db.Event, error) {
		return s.Storage.DetachProjectAlias(ctx, p)
	})
}

func (s *retryingStorage) CreateIssue(ctx context.Context, p db.CreateIssueParams) (db.Issue, db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.Issue, db.Event, error) {
		return s.Storage.CreateIssue(ctx, p)
	})
}

func (s *retryingStorage) EditIssue(ctx context.Context, p db.EditIssueParams) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, *db.Event, bool, error) {
		return s.Storage.EditIssue(ctx, p)
	})
}

func (s *retryingStorage) EditIssueAtomic(ctx context.Context, p db.EditIssueAtomicParams) (db.EditIssueAtomicResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.EditIssueAtomicResult, error) {
		return s.Storage.EditIssueAtomic(ctx, p)
	})
}

func (s *retryingStorage) CloseIssue(ctx context.Context, issueID int64, reason, actor, message string, evidence []db.Evidence) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, *db.Event, bool, error) {
		return s.Storage.CloseIssue(ctx, issueID, reason, actor, message, evidence)
	})
}

func (s *retryingStorage) CloseIssueWithEvents(ctx context.Context, issueID int64, reason, actor, message string, evidence []db.Evidence) (db.Issue, []db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, []db.Event, bool, error) {
		return s.Storage.CloseIssueWithEvents(ctx, issueID, reason, actor, message, evidence)
	})
}

func (s *retryingStorage) ReopenIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, *db.Event, bool, error) {
		return s.Storage.ReopenIssue(ctx, issueID, actor)
	})
}

func (s *retryingStorage) SoftDeleteIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, *db.Event, bool, error) {
		return s.Storage.SoftDeleteIssue(ctx, issueID, actor)
	})
}

func (s *retryingStorage) RestoreIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, *db.Event, bool, error) {
		return s.Storage.RestoreIssue(ctx, issueID, actor)
	})
}

func (s *retryingStorage) PurgeIssue(ctx context.Context, issueID int64, actor string, reason *string) (db.PurgeLog, error) {
	return retryWrite1(ctx, s.Storage, func() (db.PurgeLog, error) {
		return s.Storage.PurgeIssue(ctx, issueID, actor, reason)
	})
}

func (s *retryingStorage) ClaimOwner(ctx context.Context, issueID int64, actor string, force bool) (db.ClaimResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.ClaimResult, error) {
		return s.Storage.ClaimOwner(ctx, issueID, actor, force)
	})
}

func (s *retryingStorage) UpdateOwner(ctx context.Context, issueID int64, newOwner *string, actor string) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, *db.Event, bool, error) {
		return s.Storage.UpdateOwner(ctx, issueID, newOwner, actor)
	})
}

func (s *retryingStorage) UpdatePriority(ctx context.Context, issueID int64, newPriority *int64, actor string) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, s.Storage, func() (db.Issue, *db.Event, bool, error) {
		return s.Storage.UpdatePriority(ctx, issueID, newPriority, actor)
	})
}

func (s *retryingStorage) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	return retryWrite1(ctx, s.Storage, func() (db.PatchIssueMetadataOut, error) {
		return s.Storage.PatchIssueMetadata(ctx, in)
	})
}

func (s *retryingStorage) CreateComment(ctx context.Context, p db.CreateCommentParams) (db.Comment, db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.Comment, db.Event, error) {
		return s.Storage.CreateComment(ctx, p)
	})
}

func (s *retryingStorage) AddLabel(ctx context.Context, issueID int64, label, author string) (db.IssueLabel, error) {
	return retryWrite1(ctx, s.Storage, func() (db.IssueLabel, error) {
		return s.Storage.AddLabel(ctx, issueID, label, author)
	})
}

func (s *retryingStorage) AddLabelAndEvent(ctx context.Context, issueID int64, ev db.LabelEventParams) (db.IssueLabel, db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.IssueLabel, db.Event, error) {
		return s.Storage.AddLabelAndEvent(ctx, issueID, ev)
	})
}

func (s *retryingStorage) RemoveLabel(ctx context.Context, issueID int64, label string) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RemoveLabel(ctx, issueID, label)
	})
}

func (s *retryingStorage) RemoveLabelAndEvent(ctx context.Context, issueID int64, ev db.LabelEventParams) (db.Event, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Event, error) {
		return s.Storage.RemoveLabelAndEvent(ctx, issueID, ev)
	})
}

func (s *retryingStorage) CreateLink(ctx context.Context, p db.CreateLinkParams) (db.Link, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Link, error) {
		return s.Storage.CreateLink(ctx, p)
	})
}

func (s *retryingStorage) CreateLinkAndEvent(ctx context.Context, p db.CreateLinkParams, ev db.LinkEventParams) (db.Link, db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.Link, db.Event, error) {
		return s.Storage.CreateLinkAndEvent(ctx, p, ev)
	})
}

func (s *retryingStorage) DeleteLinkByID(ctx context.Context, linkID int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.DeleteLinkByID(ctx, linkID)
	})
}

func (s *retryingStorage) DeleteLinkAndEvent(ctx context.Context, link db.Link, ev db.LinkEventParams) (db.Event, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Event, error) {
		return s.Storage.DeleteLinkAndEvent(ctx, link, ev)
	})
}

func (s *retryingStorage) CreateRecurrence(ctx context.Context, in db.CreateRecurrenceIn) (db.Recurrence, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Recurrence, error) {
		return s.Storage.CreateRecurrence(ctx, in)
	})
}

func (s *retryingStorage) PatchRecurrence(ctx context.Context, in db.PatchRecurrenceIn) (db.PatchRecurrenceOut, error) {
	return retryWrite1(ctx, s.Storage, func() (db.PatchRecurrenceOut, error) {
		return s.Storage.PatchRecurrence(ctx, in)
	})
}

func (s *retryingStorage) SoftDeleteRecurrence(ctx context.Context, id int64, actor string) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.SoftDeleteRecurrence(ctx, id, actor)
	})
}

func (s *retryingStorage) MaterializeNext(ctx context.Context, recurrenceID int64, afterKey, actor string) (db.MaterializeNextOut, error) {
	return retryWrite1(ctx, s.Storage, func() (db.MaterializeNextOut, error) {
		return s.Storage.MaterializeNext(ctx, recurrenceID, afterKey, actor)
	})
}

func (s *retryingStorage) InsertCloseThrottledEvent(ctx context.Context, issueID int64, actor string, payload db.CloseThrottledPayload) (db.Event, error) {
	return retryWrite1(ctx, s.Storage, func() (db.Event, error) {
		return s.Storage.InsertCloseThrottledEvent(ctx, issueID, actor, payload)
	})
}

func (s *retryingStorage) ImportBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.ImportBatchResult, []db.Event, error) {
		return s.Storage.ImportBatch(ctx, p)
	})
}

func (s *retryingStorage) UpsertImportMapping(ctx context.Context, p db.ImportMappingParams) (db.ImportMapping, error) {
	return retryWrite1(ctx, s.Storage, func() (db.ImportMapping, error) {
		return s.Storage.UpsertImportMapping(ctx, p)
	})
}

func (s *retryingStorage) ImportReplay(ctx context.Context, recs []db.ImportRecord, opts db.ImportOptions) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ImportReplay(ctx, recs, opts)
	})
}

func (s *retryingStorage) EnsureSystemProject(ctx context.Context) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.EnsureSystemProject(ctx)
	})
}

func (s *retryingStorage) CreateAPIToken(ctx context.Context, p db.CreateAPITokenParams) (db.APIToken, db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.APIToken, db.Event, error) {
		return s.Storage.CreateAPIToken(ctx, p)
	})
}

func (s *retryingStorage) RevokeAPIToken(ctx context.Context, id int64, adminActor string) (db.APIToken, db.Event, error) {
	return retryWrite2(ctx, s.Storage, func() (db.APIToken, db.Event, error) {
		return s.Storage.RevokeAPIToken(ctx, id, adminActor)
	})
}

func (s *retryingStorage) AcquireClaim(ctx context.Context, p db.AcquireClaimParams) (db.LeaseResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.LeaseResult, error) {
		return s.Storage.AcquireClaim(ctx, p)
	})
}

func (s *retryingStorage) RenewClaim(ctx context.Context, p db.RenewClaimParams) (db.LeaseResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.LeaseResult, error) {
		return s.Storage.RenewClaim(ctx, p)
	})
}

func (s *retryingStorage) ReleaseClaim(ctx context.Context, p db.ReleaseClaimParams) (db.LeaseResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.LeaseResult, error) {
		return s.Storage.ReleaseClaim(ctx, p)
	})
}

func (s *retryingStorage) ForceReleaseClaim(ctx context.Context, p db.ForceReleaseClaimParams) (db.LeaseResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.LeaseResult, error) {
		return s.Storage.ForceReleaseClaim(ctx, p)
	})
}

func (s *retryingStorage) EnqueuePendingClaim(ctx context.Context, p db.PendingClaimParams) (db.PendingClaimRequest, error) {
	return retryWrite1(ctx, s.Storage, func() (db.PendingClaimRequest, error) {
		return s.Storage.EnqueuePendingClaim(ctx, p)
	})
}

func (s *retryingStorage) ResolvePendingClaim(ctx context.Context, requestUID string, claim db.IssueClaim) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ResolvePendingClaim(ctx, requestUID, claim)
	})
}

func (s *retryingStorage) RejectPendingClaim(ctx context.Context, requestUID, reason string, now time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RejectPendingClaim(ctx, requestUID, reason, now)
	})
}

func (s *retryingStorage) MarkPendingClaimAttempt(ctx context.Context, requestUID, lastError string, now time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.MarkPendingClaimAttempt(ctx, requestUID, lastError, now)
	})
}

func (s *retryingStorage) MarkClaimStatusRefreshError(ctx context.Context, projectID int64, issueUID string, statusCode int, lastError string, now time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.MarkClaimStatusRefreshError(ctx, projectID, issueUID, statusCode, lastError, now)
	})
}

func (s *retryingStorage) ClearClaimStatusRefreshError(ctx context.Context, projectID int64, issueUID string) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ClearClaimStatusRefreshError(ctx, projectID, issueUID)
	})
}

func (s *retryingStorage) UpsertClaimCache(ctx context.Context, claim db.IssueClaim) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.UpsertClaimCache(ctx, claim)
	})
}

func (s *retryingStorage) ApplyClaimStatus(ctx context.Context, projectID int64, issueUID string, status db.ClaimStatus) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ApplyClaimStatus(ctx, projectID, issueUID, status)
	})
}

func (s *retryingStorage) ExpireTimedClaims(ctx context.Context, now time.Time, limit int) ([]db.Event, error) {
	return retryWrite1(ctx, s.Storage, func() ([]db.Event, error) {
		return s.Storage.ExpireTimedClaims(ctx, now, limit)
	})
}

func (s *retryingStorage) ExpireTimedClaimsForProject(ctx context.Context, projectID int64, now time.Time, limit int) ([]db.Event, error) {
	return retryWrite1(ctx, s.Storage, func() ([]db.Event, error) {
		return s.Storage.ExpireTimedClaimsForProject(ctx, projectID, now, limit)
	})
}

func (s *retryingStorage) RecordFederationSyncPullStarted(ctx context.Context, projectID int64, at time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RecordFederationSyncPullStarted(ctx, projectID, at)
	})
}

func (s *retryingStorage) RecordFederationSyncPullSuccess(ctx context.Context, projectID int64, at time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RecordFederationSyncPullSuccess(ctx, projectID, at)
	})
}

func (s *retryingStorage) RecordFederationSyncPushStarted(ctx context.Context, projectID int64, at time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RecordFederationSyncPushStarted(ctx, projectID, at)
	})
}

func (s *retryingStorage) RecordFederationSyncPushSuccess(ctx context.Context, projectID int64, at time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RecordFederationSyncPushSuccess(ctx, projectID, at)
	})
}

func (s *retryingStorage) RecordFederationSyncReset(ctx context.Context, projectID int64, at time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RecordFederationSyncReset(ctx, projectID, at)
	})
}

func (s *retryingStorage) RecordFederationSyncError(ctx context.Context, projectID int64, syncErr error, at time.Time) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RecordFederationSyncError(ctx, projectID, syncErr, at)
	})
}

func (s *retryingStorage) ClearFederationSyncError(ctx context.Context, projectID int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ClearFederationSyncError(ctx, projectID)
	})
}

func (s *retryingStorage) RecordFederationQuarantine(ctx context.Context, p db.RecordFederationQuarantineParams) (db.FederationQuarantine, error) {
	return retryWrite1(ctx, s.Storage, func() (db.FederationQuarantine, error) {
		return s.Storage.RecordFederationQuarantine(ctx, p)
	})
}

func (s *retryingStorage) SkipFederationQuarantine(ctx context.Context, p db.SkipFederationQuarantineParams) (db.FederationQuarantine, error) {
	return retryWrite1(ctx, s.Storage, func() (db.FederationQuarantine, error) {
		return s.Storage.SkipFederationQuarantine(ctx, p)
	})
}

func (s *retryingStorage) UpsertFederationBinding(ctx context.Context, b db.FederationBinding) (db.FederationBinding, error) {
	return retryWrite1(ctx, s.Storage, func() (db.FederationBinding, error) {
		return s.Storage.UpsertFederationBinding(ctx, b)
	})
}

func (s *retryingStorage) AdoptProjectIntoFederation(ctx context.Context, p db.AdoptProjectIntoFederationParams) (db.AdoptProjectIntoFederationResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.AdoptProjectIntoFederationResult, error) {
		return s.Storage.AdoptProjectIntoFederation(ctx, p)
	})
}

func (s *retryingStorage) AdvanceFederationPullCursor(ctx context.Context, projectID, nextCursor int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.AdvanceFederationPullCursor(ctx, projectID, nextCursor)
	})
}

func (s *retryingStorage) InsertRemoteEvent(ctx context.Context, projectID int64, ev db.RemoteEvent) (bool, error) {
	return retryWrite1(ctx, s.Storage, func() (bool, error) {
		return s.Storage.InsertRemoteEvent(ctx, projectID, ev)
	})
}

func (s *retryingStorage) EnableProjectFederation(ctx context.Context, projectID int64, actor string) (db.FederationBinding, error) {
	return retryWrite1(ctx, s.Storage, func() (db.FederationBinding, error) {
		return s.Storage.EnableProjectFederation(ctx, projectID, actor)
	})
}

func (s *retryingStorage) RefreshProjectFederationBaseline(ctx context.Context, projectID int64, actor string) (db.FederationBinding, bool, error) {
	return retryWrite2(ctx, s.Storage, func() (db.FederationBinding, bool, error) {
		return s.Storage.RefreshProjectFederationBaseline(ctx, projectID, actor)
	})
}

func (s *retryingStorage) MaterializeFederatedProject(ctx context.Context, projectID int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.MaterializeFederatedProject(ctx, projectID)
	})
}

func (s *retryingStorage) ResetFederatedProject(ctx context.Context, projectID, replayHorizonEventID, pullCursorEventID int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ResetFederatedProject(ctx, projectID, replayHorizonEventID, pullCursorEventID)
	})
}

func (s *retryingStorage) CreateFederationEnrollment(ctx context.Context, p db.CreateFederationEnrollmentParams) (db.CreatedFederationEnrollment, error) {
	return retryWrite1(ctx, s.Storage, func() (db.CreatedFederationEnrollment, error) {
		return s.Storage.CreateFederationEnrollment(ctx, p)
	})
}

func (s *retryingStorage) RevokeFederationEnrollment(ctx context.Context, id int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.RevokeFederationEnrollment(ctx, id)
	})
}

func (s *retryingStorage) AdvanceFederationPushCursor(ctx context.Context, projectID, nextCursor int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.AdvanceFederationPushCursor(ctx, projectID, nextCursor)
	})
}

func (s *retryingStorage) EnableFederationPush(ctx context.Context, projectID int64, cursor int64) (db.FederationBinding, error) {
	return retryWrite1(ctx, s.Storage, func() (db.FederationBinding, error) {
		return s.Storage.EnableFederationPush(ctx, projectID, cursor)
	})
}

func (s *retryingStorage) ResetFederatedProjectIfNoPendingPush(ctx context.Context, projectID, replayHorizonEventID, pullCursorEventID int64, originInstanceUID string, pushCursorEventID int64) error {
	return retryWrite0(ctx, s.Storage, func() error {
		return s.Storage.ResetFederatedProjectIfNoPendingPush(ctx, projectID, replayHorizonEventID, pullCursorEventID, originInstanceUID, pushCursorEventID)
	})
}

func (s *retryingStorage) IngestFederationEvents(ctx context.Context, p db.FederationIngestParams) (db.FederationIngestResult, error) {
	return retryWrite1(ctx, s.Storage, func() (db.FederationIngestResult, error) {
		return s.Storage.IngestFederationEvents(ctx, p)
	})
}
