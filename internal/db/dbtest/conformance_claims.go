package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkClaimLeaseLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "claim-lease-project")
	if err != nil {
		return err
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "claimed work", Author: "worker",
	})
	if err != nil {
		return err
	}
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "second claimed work", Author: "worker",
	})
	if err != nil {
		return err
	}
	instanceUID, err := uid.New()
	if err != nil {
		return err
	}
	otherInstanceUID, err := uid.New()
	if err != nil {
		return err
	}
	principal := db.ClaimPrincipal{HolderInstanceUID: instanceUID, Holder: "alice", ClientKind: "agent"}
	other := db.ClaimPrincipal{HolderInstanceUID: otherInstanceUID, Holder: "bob", ClientKind: "agent"}
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)

	status, err := store.ClaimStatusReadOnly(ctx, project.ID, issue.ShortID, now)
	if err != nil {
		return fmt.Errorf("initial claim status: %w", err)
	}
	assert.False(t, status.Held)
	hard, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: principal,
		ClaimKind: "hard", Purpose: "edit", Now: now,
	})
	if err != nil {
		return fmt.Errorf("acquire hard claim: %w", err)
	}
	assert.True(t, hard.Granted)
	require.NotNil(t, hard.Claim)
	require.NotNil(t, hard.Event)
	assert.Equal(t, "claim.acquired", hard.Event.Type)
	assert.Equal(t, principal, hard.Holder)
	assert.Equal(t, int64(1), hard.Claim.Revision)
	repeated, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
		ClaimKind: "hard", Purpose: "edit", Now: now.Add(time.Second),
	})
	if err != nil {
		return fmt.Errorf("repeat hard claim: %w", err)
	}
	assert.True(t, repeated.Granted)
	assert.Nil(t, repeated.Event)
	assert.Equal(t, hard.Claim.ClaimUID, repeated.Claim.ClaimUID)
	denied, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: other,
		ClaimKind: "hard", Now: now.Add(2 * time.Second),
	})
	assert.ErrorIs(t, err, db.ErrClaimDenied)
	assert.False(t, denied.Granted)
	assert.Equal(t, principal, denied.Holder)
	_, err = store.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: principal,
		TTL: time.Hour, Now: now.Add(time.Minute),
	})
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	_, err = store.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: other, Now: now.Add(time.Minute),
	})
	assert.ErrorIs(t, err, db.ErrClaimNotHeld)
	released, err := store.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: principal,
		Reason: "complete", Now: now.Add(time.Minute),
	})
	if err != nil {
		return fmt.Errorf("release hard claim: %w", err)
	}
	assert.True(t, released.Granted)
	require.NotNil(t, released.Event)
	assert.Equal(t, "claim.released", released.Event.Type)
	require.NotNil(t, released.Claim.ReleasedAt)

	timed, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: principal,
		ClaimKind: "timed", TTL: time.Hour, Purpose: "timed", Now: now.Add(2 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("acquire timed claim: %w", err)
	}
	require.NotNil(t, timed.Claim.ExpiresAt)
	renewed, err := store.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: principal,
		TTL: 2 * time.Hour, Now: now.Add(150 * time.Minute),
	})
	if err != nil {
		return fmt.Errorf("renew timed claim: %w", err)
	}
	assert.True(t, renewed.Granted)
	assert.Equal(t, int64(2), renewed.Claim.Revision)
	readOnlyExpired, err := store.ClaimStatusReadOnly(ctx, project.ID, issue.ShortID, now.Add(5*time.Hour))
	if err != nil {
		return err
	}
	assert.True(t, readOnlyExpired.Held, "read-only status must not expire cached claims")
	status, err = store.ClaimStatus(ctx, project.ID, issue.ShortID, now.Add(5*time.Hour))
	if err != nil {
		return fmt.Errorf("expiring claim status: %w", err)
	}
	assert.False(t, status.Held)
	require.Len(t, status.Events, 1)
	assert.Equal(t, "claim.expired", status.Events[0].Type)

	for _, target := range []db.Issue{issue, second} {
		_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: project.ID, IssueRef: target.ShortID, Principal: principal,
			ClaimKind: "timed", TTL: time.Hour, Now: now.Add(6 * time.Hour),
		})
		if err != nil {
			return fmt.Errorf("acquire expiration fixture: %w", err)
		}
	}
	events, err := store.ExpireTimedClaimsForProject(ctx, project.ID, now.Add(8*time.Hour), 1)
	if err != nil {
		return fmt.Errorf("expire one project claim: %w", err)
	}
	require.Len(t, events, 1)
	events, err = store.ExpireTimedClaims(ctx, now.Add(8*time.Hour), 0)
	if err != nil {
		return fmt.Errorf("expire remaining claims: %w", err)
	}
	require.Len(t, events, 1)

	_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: principal,
		ClaimKind: "hard", Now: now.Add(9 * time.Hour),
	})
	if err != nil {
		return err
	}
	forced, err := store.ForceReleaseClaim(ctx, db.ForceReleaseClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Actor: "admin",
		Reason: "reassign", Now: now.Add(10 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("force release claim: %w", err)
	}
	assert.True(t, forced.Granted)
	require.NotNil(t, forced.Event)
	assert.Equal(t, "claim.force_released", forced.Event.Type)
	liveCount, err := store.CountLiveClaims(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, liveCount)
	pendingCount, err := store.CountPendingClaims(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, pendingCount)
	_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: db.ClaimPrincipal{Holder: "missing-instance"}, ClaimKind: "hard", Now: now,
	})
	assert.ErrorIs(t, err, db.ErrClaimValidation)

	deleted, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "deleted claimed work", Author: "worker",
	})
	if err != nil {
		return err
	}
	deletedClaim, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: deleted.ShortID, Principal: principal,
		ClaimKind: "hard", Now: now.Add(11 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("claim issue before deletion: %w", err)
	}
	if _, _, _, err := store.SoftDeleteIssue(ctx, deleted.ID, "worker"); err != nil {
		return err
	}
	status, err = store.ClaimStatusReadOnly(ctx, project.ID, deleted.UID, now.Add(12*time.Hour))
	if err != nil {
		return fmt.Errorf("read deleted issue claim: %w", err)
	}
	assert.True(t, status.Held)
	assert.Equal(t, deletedClaim.Claim.ClaimUID, status.Claim.ClaimUID)
	if _, err := store.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: project.ID, IssueRef: deleted.UID, Principal: principal,
		Reason: "handoff", Now: now.Add(12 * time.Hour),
	}); err != nil {
		return fmt.Errorf("release deleted issue claim: %w", err)
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: deleted.UID, Principal: principal,
		ClaimKind: "timed", TTL: time.Hour, Now: now.Add(13 * time.Hour),
	}); err != nil {
		return fmt.Errorf("reacquire deleted issue claim: %w", err)
	}
	if _, err := store.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: project.ID, IssueRef: deleted.UID, Principal: principal,
		TTL: time.Hour, Now: now.Add(13*time.Hour + 30*time.Minute),
	}); err != nil {
		return fmt.Errorf("renew deleted issue claim: %w", err)
	}
	events, err = store.ExpireTimedClaimsForProject(ctx, project.ID, now.Add(15*time.Hour), 0)
	if err != nil {
		return fmt.Errorf("expire deleted issue claim: %w", err)
	}
	require.Len(t, events, 1)
	assert.Equal(t, "system", events[0].Actor)

	background, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "background expiring claim", Author: "worker",
	})
	if err != nil {
		return err
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: background.UID, Principal: principal,
		ClaimKind: "timed", TTL: time.Hour, Now: now.Add(16 * time.Hour),
	}); err != nil {
		return err
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
		ClaimKind: "timed", TTL: 3 * time.Hour, Now: now.Add(16 * time.Hour),
	}); err != nil {
		return err
	}
	renewed, err = store.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
		TTL: time.Hour, Now: now.Add(18 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("renew while another claim expires: %w", err)
	}
	require.Len(t, renewed.Events, 1)
	assert.Equal(t, "claim.expired", renewed.Events[0].Type)
	assert.Equal(t, "system", renewed.Events[0].Actor)
	assert.Equal(t, background.UID, *renewed.Events[0].IssueUID)

	_, err = store.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: other,
		Now: now.Add(20 * time.Hour),
	})
	assert.ErrorIs(t, err, db.ErrClaimNotHeld)
	status, err = store.ClaimStatusReadOnly(ctx, project.ID, issue.UID, now.Add(20*time.Hour))
	if err != nil {
		return err
	}
	assert.True(t, status.Held, "a non-holder must not commit expiry of another principal's claim")
	expiredRelease, err := store.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
		Now: now.Add(20 * time.Hour),
	})
	assert.ErrorIs(t, err, db.ErrClaimExpired)
	require.Len(t, expiredRelease.Events, 1)
	assert.Equal(t, "system", expiredRelease.Events[0].Actor)

	hubProject, err := store.CreateProject(ctx, "claim-close-hub-project")
	if err != nil {
		return err
	}
	if _, err := store.EnableProjectFederation(ctx, hubProject.ID, "operator"); err != nil {
		return err
	}
	claimedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID, Title: "close releases claim", Author: "worker",
	})
	if err != nil {
		return err
	}
	localPrincipal := db.ClaimPrincipal{
		HolderInstanceUID: store.InstanceUID(), Holder: "worker", ClientKind: "agent",
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: hubProject.ID, IssueRef: claimedIssue.UID, Principal: localPrincipal,
		ClaimKind: "hard", Now: now.Add(21 * time.Hour),
	}); err != nil {
		return err
	}
	_, closeEvents, changed, err := store.CloseIssueWithEvents(
		ctx, claimedIssue.ID, "done", "worker", "completed", nil,
	)
	if err != nil {
		return err
	}
	assert.True(t, changed)
	require.Len(t, closeEvents, 2)
	assert.Equal(t, []string{"issue.closed", "claim.released"},
		[]string{closeEvents[0].Type, closeEvents[1].Type})
	claimStatus, err := store.ClaimStatusReadOnly(ctx, hubProject.ID, claimedIssue.UID, now.Add(22*time.Hour))
	if err != nil {
		return err
	}
	assert.False(t, claimStatus.Held)

	violatedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID, Title: "close records uncovered work", Author: "worker",
	})
	if err != nil {
		return err
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: hubProject.ID, IssueRef: violatedIssue.UID, Principal: localPrincipal,
		ClaimKind: "hard", Now: now.Add(23 * time.Hour),
	}); err != nil {
		return err
	}
	_, violationEvents, changed, err := store.CloseIssueWithEvents(
		ctx, violatedIssue.ID, "done", "different-worker", "completed elsewhere", nil,
	)
	if err != nil {
		return err
	}
	assert.True(t, changed)
	require.Len(t, violationEvents, 3)
	assert.Equal(t, []string{"issue.closed", "claim.violated", "claim.released"},
		[]string{violationEvents[0].Type, violationEvents[1].Type, violationEvents[2].Type})
	var violationPayload struct {
		OffendingEventUID string `json:"offending_event_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(violationEvents[1].Payload), &violationPayload))
	assert.Equal(t, violationEvents[0].UID, violationPayload.OffendingEventUID)
	return nil
}

func checkPendingClaimAndCacheLifecycle(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "pending-claim-project")
	if err != nil {
		return err
	}
	otherProject, err := store.CreateProject(ctx, "pending-claim-other")
	if err != nil {
		return err
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "offline claimed work", Author: "worker",
	})
	if err != nil {
		return err
	}
	deletedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "deleted pending work", Author: "worker",
	})
	if err != nil {
		return err
	}
	otherIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: otherProject.ID, Title: "other pending work", Author: "worker",
	})
	if err != nil {
		return err
	}
	firstInstance, err := uid.New()
	if err != nil {
		return err
	}
	secondInstance, err := uid.New()
	if err != nil {
		return err
	}
	thirdInstance, err := uid.New()
	if err != nil {
		return err
	}
	first := db.ClaimPrincipal{HolderInstanceUID: firstInstance, Holder: "alice", ClientKind: "agent"}
	second := db.ClaimPrincipal{HolderInstanceUID: secondInstance, Holder: "bob", ClientKind: "cli"}
	third := db.ClaimPrincipal{HolderInstanceUID: thirdInstance, Holder: "carol", ClientKind: "agent"}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	pending, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: first,
		ClaimKind: "hard", Purpose: "offline", Now: now,
	})
	if err != nil {
		return fmt.Errorf("enqueue pending claim: %w", err)
	}
	assert.Equal(t, issue.UID, pending.IssueUID)
	assert.Equal(t, first.HolderInstanceUID, pending.HolderInstanceUID)
	assert.Nil(t, pending.TTLSeconds)
	repeated, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: first,
		ClaimKind: "hard", Purpose: "ignored retry", Now: now.Add(time.Second),
	})
	if err != nil {
		return err
	}
	assert.Equal(t, pending.RequestUID, repeated.RequestUID)
	assert.Equal(t, "offline", repeated.Purpose)
	timedPending, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: second,
		ClaimKind: "timed", TTL: 90 * time.Minute, Purpose: "review", Now: now.Add(time.Minute),
	})
	if err != nil {
		return err
	}
	require.NotNil(t, timedPending.TTLSeconds)
	assert.Equal(t, int64(5400), *timedPending.TTLSeconds)
	deletedPending, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: deletedIssue.ShortID, Principal: third,
		ClaimKind: "hard", Now: now.Add(2 * time.Minute),
	})
	if err != nil {
		return err
	}
	otherPending, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: otherProject.ID, IssueRef: otherIssue.ShortID, Principal: third,
		ClaimKind: "hard", Now: now.Add(3 * time.Minute),
	})
	if err != nil {
		return err
	}
	assert.NotEmpty(t, otherPending.RequestUID)
	if backend.SeedLegacyPendingClaim == nil {
		return errors.New("backend must seed legacy pending claim fixtures")
	}
	if err := backend.SeedLegacyPendingClaim(ctx, store, otherPending.RequestUID); err != nil {
		return fmt.Errorf("seed legacy pending claim: %w", err)
	}
	legacyRepeat, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: otherProject.ID, IssueRef: otherIssue.UID, Principal: third,
		ClaimKind: "hard", Purpose: "must not duplicate", Now: now.Add(4 * time.Minute),
	})
	if err != nil {
		return fmt.Errorf("repeat imported legacy pending claim: %w", err)
	}
	assert.Equal(t, otherPending.RequestUID, legacyRepeat.RequestUID)
	assert.Empty(t, legacyRepeat.HolderInstanceUID)

	listed, err := store.ListPendingClaimRequests(ctx, project.ID, 0)
	if err != nil {
		return err
	}
	require.Len(t, listed, 3)
	assert.Equal(t, []string{pending.RequestUID, timedPending.RequestUID, deletedPending.RequestUID},
		[]string{listed[0].RequestUID, listed[1].RequestUID, listed[2].RequestUID})
	issueListed, err := store.ListPendingClaimRequestsForIssue(ctx, project.ID, issue.UID, 1)
	if err != nil {
		return err
	}
	require.Len(t, issueListed, 1)
	assert.Equal(t, timedPending.RequestUID, issueListed[0].RequestUID)
	attemptAt := now.Add(4 * time.Minute)
	if err := store.MarkPendingClaimAttempt(ctx, timedPending.RequestUID, "hub unavailable", attemptAt); err != nil {
		return err
	}
	issueListed, err = store.ListPendingClaimRequestsForIssue(ctx, project.ID, issue.UID, 0)
	if err != nil {
		return err
	}
	require.Len(t, issueListed, 2)
	require.NotNil(t, issueListed[0].LastAttemptAt)
	assert.Equal(t, attemptAt, *issueListed[0].LastAttemptAt)
	require.NotNil(t, issueListed[0].LastError)
	assert.Equal(t, "hub unavailable", *issueListed[0].LastError)
	if err := store.RejectPendingClaim(ctx, timedPending.RequestUID, "busy", now.Add(5*time.Minute)); err != nil {
		return err
	}
	issueListed, err = store.ListPendingClaimRequestsForIssue(ctx, project.ID, issue.UID, 0)
	if err != nil {
		return err
	}
	require.Len(t, issueListed, 1)
	assert.Equal(t, pending.RequestUID, issueListed[0].RequestUID)
	assert.ErrorIs(t, store.MarkPendingClaimAttempt(ctx, "missing", "retry", now), db.ErrNotFound)
	assert.ErrorIs(t, store.RejectPendingClaim(ctx, "missing", "reject", now), db.ErrNotFound)

	claimUID, err := uid.New()
	if err != nil {
		return err
	}
	resolvedClaim := db.IssueClaim{
		ClaimUID: claimUID, IssueUID: issue.UID, Holder: first.Holder,
		HolderInstanceUID: first.HolderInstanceUID, ClientKind: first.ClientKind,
		Purpose: "offline", ClaimKind: "hard", AcquiredAt: now.Add(6 * time.Minute),
		UpdatedAt: now.Add(6 * time.Minute), Revision: 1,
	}
	wrongClaim := resolvedClaim
	wrongClaim.Holder = "mallory"
	assert.ErrorIs(t, store.ResolvePendingClaim(ctx, pending.RequestUID, wrongClaim), db.ErrClaimValidation)
	if err := store.ResolvePendingClaim(ctx, pending.RequestUID, resolvedClaim); err != nil {
		return fmt.Errorf("resolve pending claim: %w", err)
	}
	if err := store.ResolvePendingClaim(ctx, pending.RequestUID, resolvedClaim); err != nil {
		return fmt.Errorf("repeat pending resolution: %w", err)
	}
	status, err := store.ClaimStatusReadOnly(ctx, project.ID, issue.ShortID, now.Add(7*time.Minute))
	if err != nil {
		return err
	}
	assert.True(t, status.Held)
	require.NotNil(t, status.Claim)
	assert.Equal(t, claimUID, status.Claim.ClaimUID)
	assert.NoError(t, store.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: project.ID, IssueRef: issue.ShortID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: first.HolderInstanceUID, Holder: first.Holder, ClientKind: "other"},
		Now:       now.Add(7 * time.Minute),
	}))
	assert.ErrorIs(t, store.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: second, Now: now.Add(7 * time.Minute),
	}), db.ErrClaimDenied)

	stale := resolvedClaim
	stale.Purpose = "stale"
	stale.Revision = 0
	stale.UpdatedAt = now.Add(5 * time.Minute)
	if err := store.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: first, Claim: &stale, HubNow: now.Add(5 * time.Minute),
	}); err != nil {
		return err
	}
	status, err = store.ClaimStatusReadOnly(ctx, project.ID, issue.UID, now)
	if err != nil {
		return err
	}
	assert.Equal(t, "offline", status.Claim.Purpose)
	newer := resolvedClaim
	newer.Purpose = "refreshed"
	newer.Revision = 2
	newer.UpdatedAt = now.Add(8 * time.Minute)
	if err := store.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: first, Claim: &newer, HubNow: newer.UpdatedAt,
	}); err != nil {
		return err
	}
	status, err = store.ClaimStatusReadOnly(ctx, project.ID, issue.UID, now)
	if err != nil {
		return err
	}
	assert.Equal(t, "refreshed", status.Claim.Purpose)
	assert.Equal(t, int64(2), status.Claim.Revision)

	replacementUID, err := uid.New()
	if err != nil {
		return err
	}
	replacement := db.IssueClaim{
		ClaimUID: replacementUID, ProjectID: project.ID, IssueUID: issue.UID,
		Holder: second.Holder, HolderInstanceUID: second.HolderInstanceUID, ClientKind: second.ClientKind,
		ClaimKind: "hard", Purpose: "replacement", AcquiredAt: now.Add(9 * time.Minute),
		UpdatedAt: now.Add(9 * time.Minute), Revision: 1,
	}
	if err := store.UpsertClaimCache(ctx, replacement); err != nil {
		return err
	}
	assert.NoError(t, store.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: second, Now: now.Add(10 * time.Minute),
	}))
	if err := store.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: false, HubNow: now.Add(11 * time.Minute),
	}); err != nil {
		return err
	}
	assert.NoError(t, store.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: first, Now: now.Add(12 * time.Minute),
	}))

	timedUID, err := uid.New()
	if err != nil {
		return err
	}
	expiresAt := now.Add(13 * time.Minute)
	timedCache := db.IssueClaim{
		ClaimUID: timedUID, ProjectID: project.ID, IssueUID: issue.UID,
		Holder: second.Holder, HolderInstanceUID: second.HolderInstanceUID, ClientKind: second.ClientKind,
		ClaimKind: "timed", AcquiredAt: now.Add(12 * time.Minute), ExpiresAt: &expiresAt,
		UpdatedAt: now.Add(12 * time.Minute), Revision: 1,
	}
	if err := store.UpsertClaimCache(ctx, timedCache); err != nil {
		return err
	}
	assert.NoError(t, store.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: project.ID, IssueRef: issue.ShortID, Principal: first, Now: now.Add(14 * time.Minute),
	}))

	refreshAt := now.Add(15 * time.Minute)
	if err := store.MarkClaimStatusRefreshError(ctx, project.ID, issue.UID, 503, "offline", refreshAt); err != nil {
		return err
	}
	refreshError, err := store.ClaimStatusRefreshError(ctx, project.ID, issue.UID)
	if err != nil {
		return err
	}
	assert.Equal(t, 503, refreshError.StatusCode)
	assert.Equal(t, "offline", refreshError.LastError)
	assert.Equal(t, refreshAt, refreshError.LastAttemptAt)
	if err := store.ClearClaimStatusRefreshError(ctx, project.ID, issue.UID); err != nil {
		return err
	}
	_, err = store.ClaimStatusRefreshError(ctx, project.ID, issue.UID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	assert.ErrorIs(t, store.MarkClaimStatusRefreshError(ctx, 0, issue.UID, 500, "bad", now), db.ErrNotFound)

	if _, _, _, err := store.SoftDeleteIssue(ctx, deletedIssue.ID, "worker"); err != nil {
		return err
	}
	deletedClaimUID, err := uid.New()
	if err != nil {
		return err
	}
	deletedClaim := db.IssueClaim{
		ClaimUID: deletedClaimUID, IssueUID: deletedIssue.UID, Holder: third.Holder,
		HolderInstanceUID: third.HolderInstanceUID, ClientKind: third.ClientKind,
		ClaimKind: "hard", AcquiredAt: now.Add(16 * time.Minute),
		UpdatedAt: now.Add(16 * time.Minute), Revision: 1,
	}
	if err := store.ResolvePendingClaim(ctx, deletedPending.RequestUID, deletedClaim); err != nil {
		return fmt.Errorf("resolve deleted issue pending claim: %w", err)
	}
	deletedClaim.Purpose = "refreshed after deletion"
	deletedClaim.Revision = 2
	deletedClaim.UpdatedAt = now.Add(17 * time.Minute)
	if err := store.ApplyClaimStatus(ctx, project.ID, deletedIssue.UID, db.ClaimStatus{
		Held: true, Holder: third, Claim: &deletedClaim, HubNow: deletedClaim.UpdatedAt,
	}); err != nil {
		return fmt.Errorf("refresh deleted issue claim: %w", err)
	}
	deletedStatus, err := store.ClaimStatusReadOnly(ctx, project.ID, deletedIssue.UID, now.Add(18*time.Minute))
	if err != nil {
		return err
	}
	require.NotNil(t, deletedStatus.Claim)
	assert.Equal(t, "refreshed after deletion", deletedStatus.Claim.Purpose)
	pendingCount, err := store.CountPendingClaims(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, pendingCount)
	otherCount, err := store.CountPendingClaims(ctx, otherProject.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(1), otherCount)

	filter := db.ExportFilter{ProjectID: &project.ID}
	pendingExports, err := collectExport(store.ExportPendingClaimRequests(ctx, filter))
	if err != nil {
		return err
	}
	require.Len(t, pendingExports, 2)
	assert.Equal(t, pending.RequestUID, pendingExports[0].RequestUID)
	require.NotNil(t, pendingExports[0].ResolvedAt)
	assert.Equal(t, timedPending.RequestUID, pendingExports[1].RequestUID)
	require.NotNil(t, pendingExports[1].RejectedAt)
	withDeleted := db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}
	pendingExports, err = collectExport(store.ExportPendingClaimRequests(ctx, withDeleted))
	if err != nil {
		return err
	}
	assert.Len(t, pendingExports, 3)
	claimExports, err := collectExport(store.ExportIssueClaims(ctx, filter))
	if err != nil {
		return err
	}
	require.Len(t, claimExports, 3)
	assert.Equal(t, []string{claimUID, replacementUID, timedUID},
		[]string{claimExports[0].ClaimUID, claimExports[1].ClaimUID, claimExports[2].ClaimUID})
	return nil
}

func checkClaimViolationQueries(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	if backend.SeedClaimViolation == nil {
		return errors.New("backend must seed claim violation fixtures")
	}
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "claim-violation-project")
	if err != nil {
		return err
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "audited work", Author: "author",
	})
	if err != nil {
		return err
	}
	holderUID, err := uid.New()
	if err != nil {
		return err
	}
	principal := db.ClaimPrincipal{HolderInstanceUID: holderUID, Holder: "holder", ClientKind: "agent"}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
		ClaimKind: "hard", Now: time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC),
	}); err != nil {
		return err
	}
	firstUID, err := uid.New()
	if err != nil {
		return err
	}
	firstOrigin, err := uid.New()
	if err != nil {
		return err
	}
	if err := backend.SeedClaimViolation(ctx, store, project, issue, firstUID,
		json.RawMessage(`{"offending_event_uid":"event-current","offending_event_type":"issue.updated","offending_origin_instance_uid":"`+firstOrigin+`","actor":"remote-agent","reason":"uncovered_work"}`)); err != nil {
		return err
	}
	secondUID, err := uid.New()
	if err != nil {
		return err
	}
	legacyOrigin, err := uid.New()
	if err != nil {
		return err
	}
	if err := backend.SeedClaimViolation(ctx, store, project, issue, secondUID,
		json.RawMessage(`{"event_uid":"event-legacy","event_type":"issue.commented","origin_instance_uid":"`+legacyOrigin+`","actor":"legacy-agent","reason":"uncovered_work"}`)); err != nil {
		return err
	}
	violations, count, err := store.UnresolvedClaimViolationsForIssue(ctx, project.ID, issue.UID, 1)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(2), count)
	require.Len(t, violations, 1)
	assert.Equal(t, "event-legacy", violations[0].OffendingEventUID)
	assert.Equal(t, "issue.commented", violations[0].OffendingEventType)
	assert.Equal(t, legacyOrigin, violations[0].OffendingOriginInstanceUID)
	assert.Equal(t, issue.ShortID, violations[0].IssueShortID)
	projectViolations, projectCount, err := store.UnresolvedClaimViolationsForProject(ctx, project.ID, 10)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(2), projectCount)
	require.Len(t, projectViolations, 2)
	assert.Equal(t, []string{"event-legacy", "event-current"},
		[]string{projectViolations[0].OffendingEventUID, projectViolations[1].OffendingEventUID})
	zeroPage, zeroCount, err := store.UnresolvedClaimViolationsForIssue(ctx, project.ID, issue.UID, -1)
	if err != nil {
		return err
	}
	assert.Empty(t, zeroPage)
	assert.Equal(t, int64(2), zeroCount)
	if _, err := store.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
		Reason: "complete", Now: time.Date(2026, 7, 15, 18, 5, 0, 0, time.UTC),
	}); err != nil {
		return err
	}
	violations, count, err = store.UnresolvedClaimViolationsForIssue(ctx, project.ID, issue.UID, 10)
	if err != nil {
		return err
	}
	assert.Empty(t, violations)
	assert.Zero(t, count)
	return nil
}
