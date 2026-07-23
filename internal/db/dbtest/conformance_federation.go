package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

type federationRotationResult struct {
	created db.CreatedFederationEnrollment
	err     error
}

type federationRotationStageBarrier struct {
	arrivals chan struct{}
	release  chan struct{}
	calls    atomic.Int64
}

func newFederationRotationStageBarrier() *federationRotationStageBarrier {
	return &federationRotationStageBarrier{
		arrivals: make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (b *federationRotationStageBarrier) afterLookup(ctx context.Context) error {
	if b.calls.Add(1) > 2 {
		return nil
	}
	select {
	case b.arrivals <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runConcurrentFederationRotations(
	t *testing.T,
	store db.Storage,
	backend Backend,
	params []db.CreateFederationEnrollmentParams,
) [2]federationRotationResult {
	t.Helper()
	require.Len(t, params, 2)
	require.NotNil(t, backend.InstallEnrollmentRotationStage)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	barrier := newFederationRotationStageBarrier()
	removeStage := backend.InstallEnrollmentRotationStage(store, barrier.afterLookup)
	defer removeStage()

	results := make(chan federationRotationResult, 2)
	for _, input := range params {
		go func() {
			created, err := store.RotateFederationEnrollment(ctx, input)
			results <- federationRotationResult{created: created, err: err}
		}()
	}

	for range 2 {
		select {
		case <-barrier.arrivals:
		case result := <-results:
			require.FailNow(
				t,
				"rotation completed before both transactions passed replacement lookup",
				"result error: %v",
				result.err,
			)
		case <-ctx.Done():
			require.FailNow(
				t,
				"wait for concurrent rotation transaction stages",
				"error: %v",
				ctx.Err(),
			)
		}
	}
	assert.Equal(t, int64(2), barrier.calls.Load())
	select {
	case result := <-results:
		require.FailNow(
			t,
			"rotation completed before transaction-stage release",
			"result error: %v",
			result.err,
		)
	default:
	}
	close(barrier.release)

	var output [2]federationRotationResult
	for i := range output {
		select {
		case output[i] = <-results:
		case <-ctx.Done():
			require.FailNow(t, "wait for concurrent rotation results", "error: %v", ctx.Err())
		}
	}
	return output
}

func checkFederationControlLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	hub, err := store.CreateProject(ctx, "federation-control-hub")
	if err != nil {
		return err
	}
	spoke, err := store.CreateProject(ctx, "federation-control-spoke")
	if err != nil {
		return err
	}
	standalone, err := store.CreateProject(ctx, "federation-control-standalone")
	if err != nil {
		return err
	}
	peer, err := store.CreateProject(ctx, "federation-control-peer")
	if err != nil {
		return err
	}
	incompatible, err := store.CreateProject(ctx, "federation-control-incompatible")
	if err != nil {
		return err
	}
	spokeIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spoke.ID, Title: "spoke relationship", Author: "member",
	})
	if err != nil {
		return err
	}
	peerIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: peer.ID, Title: "compatible relationship", Author: "member",
	})
	if err != nil {
		return err
	}
	incompatibleIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: incompatible.ID, Title: "incompatible relationship", Author: "member",
	})
	if err != nil {
		return err
	}
	bindings, err := store.ListFederationBindings(ctx)
	if err != nil {
		return err
	}
	assert.Empty(t, bindings)
	_, err = store.FederationBindingByProject(ctx, hub.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	hubBinding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: hub.ID, Role: db.FederationRoleHub, HubProjectUID: hub.UID, Enabled: true,
	})
	if err != nil {
		return fmt.Errorf("create hub federation binding: %w", err)
	}
	assert.Equal(t, db.FederationRoleHub, hubBinding.Role)
	assert.True(t, hubBinding.Enabled)
	spokeBinding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: spoke.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: hub.ID, HubProjectUID: hub.UID,
		ReplayHorizonEventID: 10, PullCursorEventID: 11, PushCursorEventID: 12,
		Actor: "sync-agent", AllowInsecure: true, Enabled: true,
	})
	if err != nil {
		return fmt.Errorf("create spoke federation binding: %w", err)
	}
	assert.Equal(t, "sync-agent", spokeBinding.Actor)
	assert.True(t, spokeBinding.AllowInsecure)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spoke.ID, Title: "must not write while pull-only", Author: "member",
	})
	assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
	_, _, _, err = store.CloseIssueWithEvents(
		ctx, spokeIssue.ID, "done", "member", "must not close while pull-only", nil,
	)
	assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
	_, err = store.PurgeIssue(ctx, spokeIssue.ID, "member", nil)
	assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: peer.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example/another-path", HubProjectID: hub.ID, HubProjectUID: hub.UID,
		Enabled: true,
	})
	if err != nil {
		return fmt.Errorf("create compatible spoke federation binding: %w", err)
	}
	if _, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: spokeIssue.ID, ToIssueID: peerIssue.ID, Type: "blocks", Author: "member",
	}); err != nil {
		return err
	}
	if _, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: spokeIssue.ID, ToIssueID: incompatibleIssue.ID, Type: "blocks", Author: "member",
	}); err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: incompatible.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://other.example", HubProjectID: hub.ID, HubProjectUID: hub.UID,
		Enabled: true,
	})
	if err != nil {
		return fmt.Errorf("create incompatible spoke federation binding: %w", err)
	}
	if _, err := store.LinkByEndpoints(ctx, spokeIssue.ID, peerIssue.ID, "blocks"); err != nil {
		return fmt.Errorf("compatible federation link was removed: %w", err)
	}
	_, err = store.LinkByEndpoints(ctx, spokeIssue.ID, incompatibleIssue.ID, "blocks")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: peer.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://other.example/moved", HubProjectID: hub.ID, HubProjectUID: hub.UID,
		Enabled: true,
	})
	if err != nil {
		return fmt.Errorf("move spoke federation binding between groups: %w", err)
	}
	_, err = store.LinkByEndpoints(ctx, spokeIssue.ID, peerIssue.ID, "blocks")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: standalone.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: hub.ID, HubProjectUID: hub.UID,
		PushEnabled: true, Enabled: true,
	})
	assert.Error(t, err)
	bindings, err = store.ListFederationBindings(ctx)
	if err != nil {
		return err
	}
	require.Len(t, bindings, 4)
	assert.Equal(t, []int64{hub.ID, spoke.ID, peer.ID, incompatible.ID},
		[]int64{bindings[0].ProjectID, bindings[1].ProjectID, bindings[2].ProjectID, bindings[3].ProjectID})

	if err := store.AdvanceFederationPullCursor(ctx, spoke.ID, 20); err != nil {
		return err
	}
	if err := store.AdvanceFederationPullCursor(ctx, spoke.ID, 15); err != nil {
		return err
	}
	if err := store.AdvanceFederationPushCursor(ctx, spoke.ID, 30); err != nil {
		return err
	}
	if err := store.AdvanceFederationPushCursor(ctx, spoke.ID, 25); err != nil {
		return err
	}
	spokeBinding, err = store.EnableFederationPush(ctx, spoke.ID, 40)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(20), spokeBinding.PullCursorEventID)
	assert.Equal(t, int64(40), spokeBinding.PushCursorEventID)
	assert.True(t, spokeBinding.PushEnabled)
	assert.ErrorIs(t, store.AdvanceFederationPullCursor(ctx, standalone.ID, 1), db.ErrNotFound)

	pullStarted := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	pullSuccess := pullStarted.Add(time.Minute)
	pushStarted := pullStarted.Add(2 * time.Minute)
	pushSuccess := pullStarted.Add(3 * time.Minute)
	resetAt := pullStarted.Add(4 * time.Minute)
	if err := store.RecordFederationSyncPullStarted(ctx, spoke.ID, pullStarted); err != nil {
		return err
	}
	if err := store.RecordFederationSyncPullSuccess(ctx, spoke.ID, pullSuccess); err != nil {
		return err
	}
	if err := store.RecordFederationSyncPushStarted(ctx, spoke.ID, pushStarted); err != nil {
		return err
	}
	if err := store.RecordFederationSyncPushSuccess(ctx, spoke.ID, pushSuccess); err != nil {
		return err
	}
	if err := store.RecordFederationSyncReset(ctx, spoke.ID, resetAt); err != nil {
		return err
	}
	if err := store.RecordFederationSyncError(ctx, spoke.ID, errors.New("transport unavailable"), resetAt.Add(time.Minute)); err != nil {
		return err
	}
	status, err := store.FederationSyncStatusByProject(ctx, spoke.ID)
	if err != nil {
		return err
	}
	require.NotNil(t, status.LastPullStartedAt)
	assert.Equal(t, pullStarted, *status.LastPullStartedAt)
	require.NotNil(t, status.LastPushSuccessAt)
	assert.Equal(t, pushSuccess, *status.LastPushSuccessAt)
	require.NotNil(t, status.LastError)
	assert.Equal(t, "transport unavailable", *status.LastError)
	if err := store.ClearFederationSyncError(ctx, spoke.ID); err != nil {
		return err
	}
	status, err = store.FederationSyncStatusByProject(ctx, spoke.ID)
	if err != nil {
		return err
	}
	assert.Nil(t, status.LastErrorAt)
	assert.Nil(t, status.LastError)
	if err := store.RecordFederationSyncPullStarted(ctx, standalone.ID, pullStarted); err != nil {
		return err
	}
	_, err = store.FederationSyncStatusByProject(ctx, standalone.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)

	spokeUID, err := uid.New()
	if err != nil {
		return err
	}
	created, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: spokeUID, ProjectID: &hub.ID, Capabilities: "push,pull", Actor: "member",
	})
	if err != nil {
		return err
	}
	assert.NotEmpty(t, created.Token)
	assert.NotEqual(t, created.Token, created.Enrollment.TokenHash)
	assert.Equal(t, db.FederationTokenHash(created.Token), created.Enrollment.TokenHash)
	assert.Equal(t, "pull,push", created.Enrollment.Capabilities)
	foundEnrollment, err := store.FindActiveFederationEnrollment(
		ctx,
		db.ActiveFederationEnrollmentParams{
			ProjectID:        hub.ID,
			SpokeInstanceUID: spokeUID,
			Capabilities:     "pull,push",
			Actor:            "member",
		},
	)
	if err != nil {
		return err
	}
	assert.Equal(t, created.Enrollment, foundEnrollment)
	_, err = store.FindActiveFederationEnrollment(
		ctx,
		db.ActiveFederationEnrollmentParams{
			ProjectID:        hub.ID,
			SpokeInstanceUID: spokeUID,
			Capabilities:     "pull,push",
			Actor:            "different member",
		},
	)
	assert.ErrorIs(t, err, db.ErrNotFound)
	wildcard, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token: "wildcard-enrollment-secret", SpokeInstanceUID: spokeUID,
		Capabilities: "pull", Actor: "member",
	})
	if err != nil {
		return err
	}
	assert.Equal(t, "wildcard-enrollment-secret", wildcard.Token)
	rollbackProject, err := store.CreateProject(ctx, "atomic-enrollment-rollback")
	if err != nil {
		return err
	}
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: rollbackProject.ID, Title: "must not become a baseline", Author: "member",
	}); err != nil {
		return err
	}
	eventsBeforeFailure, err := store.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: rollbackProject.ID, Limit: 100,
	})
	if err != nil {
		return err
	}
	_, err = store.CreateProjectFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token: "wildcard-enrollment-secret", SpokeInstanceUID: spokeUID,
		ProjectID: &rollbackProject.ID, Capabilities: "pull", Actor: "member",
	})
	require.Error(t, err, "duplicate token must fail after federation enablement begins")
	assert.ErrorIs(t, err, db.ErrFederationEnrollmentTokenConflict)
	_, err = store.FederationBindingByProject(ctx, rollbackProject.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	eventsAfterFailure, err := store.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: rollbackProject.ID, Limit: 100,
	})
	if err != nil {
		return err
	}
	assert.Equal(t, eventsBeforeFailure, eventsAfterFailure,
		"failed enrollment must roll back federation baseline events")
	_, err = store.AuthorizeFederationToken(ctx, wildcard.Token, rollbackProject.ID, "pull")
	assert.ErrorIs(t, err, db.ErrNotFound,
		"failed enrollment must not expose the project to an existing wildcard credential")
	authorized, err := store.AuthorizeFederationToken(ctx, created.Token, hub.ID, "pull")
	if err != nil {
		return err
	}
	assert.Equal(t, created.Enrollment.ID, authorized.ID)
	_, err = store.AuthorizeFederationToken(ctx, created.Token, hub.ID, "claim")
	assert.ErrorIs(t, err, db.ErrNotFound)
	authorized, err = store.AuthorizeFederationToken(ctx, wildcard.Token, hub.ID, "pull")
	if err != nil {
		return err
	}
	assert.Equal(t, wildcard.Enrollment.ID, authorized.ID)
	fence := store.FederationEnrollmentTransactionFence(authorized, hub.ID, "pull")
	_, _, err = store.CreateIssue(db.WithTransactionFence(ctx, fence), db.CreateIssueParams{
		ProjectID: hub.ID, Title: "authorized federation fence", Author: "member",
	})
	if err != nil {
		return err
	}
	_, err = store.AuthorizeFederationToken(ctx, wildcard.Token, spoke.ID, "pull")
	assert.ErrorIs(t, err, db.ErrNotFound)
	count, err := store.CountActiveFederationEnrollments(ctx, hub.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(2), count)
	enrollments, err := store.ListFederationEnrollments(ctx)
	if err != nil {
		return err
	}
	require.Len(t, enrollments, 2)
	if err := store.RevokeFederationEnrollment(ctx, created.Enrollment.ID); err != nil {
		return err
	}
	_, err = store.FindActiveFederationEnrollment(
		ctx,
		db.ActiveFederationEnrollmentParams{
			ProjectID:        hub.ID,
			SpokeInstanceUID: spokeUID,
			Capabilities:     "pull,push",
			Actor:            "member",
		},
	)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.AuthorizeFederationToken(ctx, created.Token, hub.ID, "pull")
	assert.ErrorIs(t, err, db.ErrNotFound)
	revokedFence := store.FederationEnrollmentTransactionFence(created.Enrollment, hub.ID, "pull")
	_, _, err = store.CreateIssue(db.WithTransactionFence(ctx, revokedFence), db.CreateIssueParams{
		ProjectID: hub.ID, Title: "revoked federation fence", Author: "member",
	})
	assert.ErrorIs(t, err, db.ErrNotFound)
	assert.ErrorIs(t, store.RevokeFederationEnrollment(ctx, created.Enrollment.ID+10000), db.ErrNotFound)

	t.Run("explicit enrollment token replay", func(t *testing.T) {
		replayProject, projectErr := store.CreateProject(ctx, "federation-enrollment-replay-project")
		require.NoError(t, projectErr)
		params := db.CreateFederationEnrollmentParams{
			Token: "replay-enrollment-secret", SpokeInstanceUID: spokeUID,
			ProjectID: &replayProject.ID, Capabilities: "push,pull", Actor: "member",
		}
		first, createErr := store.CreateProjectFederationEnrollment(ctx, params)
		require.NoError(t, createErr)

		sameProjectID := replayProject.ID
		params.ProjectID = &sameProjectID
		params.Capabilities = "pull,push"
		replayed, replayErr := store.CreateProjectFederationEnrollment(ctx, params)
		require.NoError(t, replayErr)
		assert.Equal(t, first.Enrollment.ID, replayed.Enrollment.ID)
		assert.Equal(t, params.Token, replayed.Token)

		enrollments, listErr := store.ListFederationEnrollments(ctx)
		require.NoError(t, listErr)
		matchingRows := 0
		for _, enrollment := range enrollments {
			if enrollment.TokenHash == db.FederationTokenHash(params.Token) {
				matchingRows++
			}
		}
		assert.Equal(t, 1, matchingRows)
	})

	t.Run("explicit wildcard enrollment token replay", func(t *testing.T) {
		replayed, replayErr := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token: "wildcard-enrollment-secret", SpokeInstanceUID: spokeUID,
			Capabilities: "pull", Actor: "member",
		})
		require.NoError(t, replayErr)
		assert.Equal(t, wildcard.Enrollment.ID, replayed.Enrollment.ID)
		assert.Nil(t, replayed.Enrollment.ProjectID)
	})

	t.Run("explicit enrollment token mismatch", func(t *testing.T) {
		otherSpokeUID, uidErr := uid.New()
		require.NoError(t, uidErr)
		base := db.CreateFederationEnrollmentParams{
			Token: "mismatch-enrollment-secret", SpokeInstanceUID: spokeUID,
			ProjectID: &peer.ID, Capabilities: "push,pull", Actor: "member",
		}
		_, createErr := store.CreateFederationEnrollment(ctx, base)
		require.NoError(t, createErr)

		for _, tc := range []struct {
			name   string
			mutate func(*db.CreateFederationEnrollmentParams)
		}{
			{name: "spoke uid", mutate: func(p *db.CreateFederationEnrollmentParams) {
				p.SpokeInstanceUID = otherSpokeUID
			}},
			{name: "project id", mutate: func(p *db.CreateFederationEnrollmentParams) {
				p.ProjectID = nil
			}},
			{name: "capabilities", mutate: func(p *db.CreateFederationEnrollmentParams) {
				p.Capabilities = "pull"
			}},
			{name: "actor", mutate: func(p *db.CreateFederationEnrollmentParams) {
				p.Actor = "other-member"
			}},
			{name: "adoption policy", mutate: func(p *db.CreateFederationEnrollmentParams) {
				p.AllowAdoptionSnapshotAuthors = true
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				candidate := base
				tc.mutate(&candidate)
				_, replayErr := store.CreateFederationEnrollment(ctx, candidate)
				assert.ErrorIs(t, replayErr, db.ErrFederationEnrollmentTokenConflict)
			})
		}
	})

	t.Run("revoked explicit enrollment token conflicts", func(t *testing.T) {
		params := db.CreateFederationEnrollmentParams{
			Token: "revoked-replay-enrollment-secret", SpokeInstanceUID: spokeUID,
			ProjectID: &peer.ID, Capabilities: "pull", Actor: "member",
		}
		first, createErr := store.CreateFederationEnrollment(ctx, params)
		require.NoError(t, createErr)
		require.NoError(t, store.RevokeFederationEnrollment(ctx, first.Enrollment.ID))

		_, replayErr := store.CreateFederationEnrollment(ctx, params)
		assert.ErrorIs(t, replayErr, db.ErrFederationEnrollmentTokenConflict)
	})

	t.Run("concurrent explicit enrollment token replay", func(t *testing.T) {
		params := db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
			Token: "concurrent-replay-enrollment-secret", SpokeInstanceUID: spokeUID,
			ProjectID: &peer.ID, Capabilities: "pull", Actor: "member",
		}
		type result struct {
			created db.CreatedFederationEnrollment
			err     error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		for range 2 {
			go func() {
				ready.Done()
				<-start
				created, createErr := store.CreateFederationEnrollment(ctx, params)
				results <- result{created: created, err: createErr}
			}()
		}
		ready.Wait()
		close(start)
		first := <-results
		second := <-results
		require.NoError(t, first.err)
		require.NoError(t, second.err)
		assert.Equal(t, first.created.Enrollment.ID, second.created.Enrollment.ID)

		enrollments, listErr := store.ListFederationEnrollments(ctx)
		require.NoError(t, listErr)
		matchingRows := 0
		for _, enrollment := range enrollments {
			if enrollment.TokenHash == db.FederationTokenHash(params.Token) {
				matchingRows++
			}
		}
		assert.Equal(t, 1, matchingRows)
	})

	t.Run("generated enrollment tokens remain distinct", func(t *testing.T) {
		params := db.CreateFederationEnrollmentParams{
			SpokeInstanceUID: spokeUID, ProjectID: &peer.ID,
			Capabilities: "pull", Actor: "member",
		}
		first, firstErr := store.CreateFederationEnrollment(ctx, params)
		require.NoError(t, firstErr)
		second, secondErr := store.CreateFederationEnrollment(ctx, params)
		require.NoError(t, secondErr)
		assert.NotEqual(t, first.Enrollment.ID, second.Enrollment.ID)
		assert.NotEqual(t, first.Token, second.Token)
		assert.Nil(t, first.Enrollment.RevokedAt)
		assert.Nil(t, second.Enrollment.RevokedAt)
	})

	quarantineAt := time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	pushQuarantine, err := store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: spoke.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: 41, LastEventID: 42, EventUIDs: []string{"event-a", "event-b"},
		Error: "rejected batch", CreatedAt: quarantineAt,
	})
	if err != nil {
		return err
	}
	assert.NotZero(t, pushQuarantine.ID)
	repeatedQuarantine, err := store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: spoke.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: 50, LastEventID: 51, EventUIDs: []string{"replacement"},
		Error: "second failure", CreatedAt: quarantineAt.Add(time.Minute),
	})
	if err != nil {
		return err
	}
	assert.Equal(t, pushQuarantine.ID, repeatedQuarantine.ID)
	assert.Equal(t, int64(42), repeatedQuarantine.LastEventID)
	active, err := store.ActiveFederationQuarantine(ctx, spoke.ID, db.FederationQuarantineDirectionPush)
	if err != nil {
		return err
	}
	assert.Equal(t, []string{"event-a", "event-b"}, active.EventUIDs)
	retried, err := store.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{
		ID: active.ID, ProjectID: spoke.ID, Actor: "operator", Reason: "transient", Now: quarantineAt.Add(2 * time.Minute),
	})
	if err != nil {
		return err
	}
	require.NotNil(t, retried.SkipReason)
	assert.Equal(t, "retry: transient", *retried.SkipReason)
	spokeBinding, err = store.FederationBindingByProject(ctx, spoke.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(40), spokeBinding.PushCursorEventID)
	pushQuarantine, err = store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: spoke.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: 43, LastEventID: 44, Error: "permanent", CreatedAt: quarantineAt.Add(3 * time.Minute),
	})
	if err != nil {
		return err
	}
	skipped, err := store.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{
		ID: pushQuarantine.ID, ProjectID: spoke.ID, Actor: "operator", Reason: "discard", Now: quarantineAt.Add(4 * time.Minute),
	})
	if err != nil {
		return err
	}
	require.NotNil(t, skipped.SkippedBy)
	assert.Equal(t, "operator", *skipped.SkippedBy)
	spokeBinding, err = store.FederationBindingByProject(ctx, spoke.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(44), spokeBinding.PushCursorEventID)
	pullQuarantine, err := store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: spoke.ID, Direction: db.FederationQuarantineDirectionPull,
		FirstEventID: 60, LastEventID: 61, Error: "bad pull", CreatedAt: quarantineAt.Add(5 * time.Minute),
	})
	if err != nil {
		return err
	}
	_, err = store.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{
		ID: pullQuarantine.ID, ProjectID: spoke.ID, Actor: "operator",
	})
	assert.ErrorIs(t, err, db.ErrFederationQuarantineRetryUnsupportedDirection)
	_, err = store.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{
		ID: pullQuarantine.ID, ProjectID: spoke.ID, Actor: "operator",
	})
	assert.Error(t, err)
	activeQuarantines, err := store.ActiveFederationQuarantinesByProject(ctx, spoke.ID)
	if err != nil {
		return err
	}
	require.Len(t, activeQuarantines, 1)
	assert.Equal(t, pullQuarantine.ID, activeQuarantines[0].ID)
	noBindingQuarantine, err := store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: standalone.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: 1, LastEventID: 1, Error: "ignored", CreatedAt: quarantineAt,
	})
	if err != nil {
		return err
	}
	assert.Zero(t, noBindingQuarantine.ID)

	bindingExports, err := collectExport(store.ExportFederationBindings(ctx, db.ExportFilter{ProjectID: &spoke.ID}))
	if err != nil {
		return err
	}
	require.Len(t, bindingExports, 1)
	assert.True(t, bindingExports[0].AllowInsecure)
	assert.True(t, bindingExports[0].PushEnabled)
	statusExports, err := collectExport(store.ExportFederationSyncStatus(ctx, db.ExportFilter{ProjectID: &spoke.ID}))
	if err != nil {
		return err
	}
	require.Len(t, statusExports, 1)
	quarantineExports, err := collectExport(store.ExportFederationQuarantine(ctx, db.ExportFilter{ProjectID: &spoke.ID}))
	if err != nil {
		return err
	}
	assert.Len(t, quarantineExports, 3)
	enrollmentExports, err := collectExport(store.ExportFederationEnrollments(ctx, db.ExportFilter{ProjectID: &hub.ID}))
	if err != nil {
		return err
	}
	require.Len(t, enrollmentExports, 1)
	assert.Equal(t, created.Enrollment.ID, enrollmentExports[0].ID)
	return nil
}

func checkFederationEnrollmentRotation(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()
	require.NotNil(t, backend.InstallEnrollmentInsertFailure)

	enrollmentByID := func(t *testing.T, id int64) db.FederationEnrollment {
		t.Helper()
		enrollments, err := store.ListFederationEnrollments(ctx)
		require.NoError(t, err)
		for _, enrollment := range enrollments {
			if enrollment.ID == id {
				return enrollment
			}
		}
		require.FailNow(t, "federation enrollment not found", "id=%d", id)
		return db.FederationEnrollment{}
	}
	enrollmentCountByToken := func(t *testing.T, token string) int {
		t.Helper()
		enrollments, err := store.ListFederationEnrollments(ctx)
		require.NoError(t, err)
		count := 0
		for _, enrollment := range enrollments {
			if enrollment.TokenHash == db.FederationTokenHash(token) {
				count++
			}
		}
		return count
	}
	newSpokeUID := func(t *testing.T) string {
		t.Helper()
		value, err := uid.New()
		require.NoError(t, err)
		return value
	}
	createEnrollment := func(
		t *testing.T,
		token string,
		spokeUID string,
		projectID *int64,
		actor string,
	) db.CreatedFederationEnrollment {
		t.Helper()
		created, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token: token, SpokeInstanceUID: spokeUID, ProjectID: projectID,
			Capabilities: "pull", Actor: actor,
		})
		require.NoError(t, err)
		return created
	}

	t.Run("rotate revokes only matching spoke and project grants", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "rotation-scope-project")
		require.NoError(t, err)
		otherProject, err := store.CreateProject(ctx, "rotation-other-project")
		require.NoError(t, err)
		spokeUID := newSpokeUID(t)
		otherSpokeUID := newSpokeUID(t)

		old := createEnrollment(t, "rotation-old-token", spokeUID, &project.ID, "member")
		secondOld := createEnrollment(t, "rotation-second-old-token", spokeUID, &project.ID, "other-member")
		wildcard := createEnrollment(t, "rotation-wildcard-token", spokeUID, nil, "member")
		otherProjectGrant := createEnrollment(
			t, "rotation-other-project-token", spokeUID, &otherProject.ID, "member",
		)
		otherSpokeGrant := createEnrollment(
			t, "rotation-other-spoke-token", otherSpokeUID, &project.ID, "member",
		)

		rotated, err := store.RotateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token: "rotation-replacement-token", SpokeInstanceUID: spokeUID, ProjectID: &project.ID,
			Capabilities: "push,pull", Actor: "member",
		})
		require.NoError(t, err)
		assert.Equal(t, "rotation-replacement-token", rotated.Token)
		assert.Nil(t, rotated.Enrollment.RevokedAt)
		assert.Equal(t, db.FederationTokenHash(rotated.Token), rotated.Enrollment.TokenHash)

		assert.NotNil(t, enrollmentByID(t, old.Enrollment.ID).RevokedAt)
		assert.NotNil(t, enrollmentByID(t, secondOld.Enrollment.ID).RevokedAt)
		assert.Nil(t, enrollmentByID(t, wildcard.Enrollment.ID).RevokedAt)
		assert.Nil(t, enrollmentByID(t, otherProjectGrant.Enrollment.ID).RevokedAt)
		assert.Nil(t, enrollmentByID(t, otherSpokeGrant.Enrollment.ID).RevokedAt)
		assert.Nil(t, enrollmentByID(t, rotated.Enrollment.ID).RevokedAt)
	})

	t.Run("rotate validates project scope and replacement token", func(t *testing.T) {
		projectID := int64(0)
		spokeUID := newSpokeUID(t)
		base := db.CreateFederationEnrollmentParams{
			Token: "rotation-validation-token", SpokeInstanceUID: spokeUID,
			ProjectID: &projectID, Capabilities: "pull", Actor: "member",
		}
		_, err := store.RotateFederationEnrollment(ctx, base)
		assert.Error(t, err)

		base.ProjectID = nil
		_, err = store.RotateFederationEnrollment(ctx, base)
		assert.Error(t, err)

		project, err := store.CreateProject(ctx, "rotation-validation-project")
		require.NoError(t, err)
		base.ProjectID = &project.ID
		base.Token = ""
		_, err = store.RotateFederationEnrollment(ctx, base)
		assert.Error(t, err)
	})

	t.Run("rotate creates replacement without an old grant", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "rotation-no-match-project")
		require.NoError(t, err)
		spokeUID := newSpokeUID(t)

		rotated, err := store.RotateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token: "rotation-no-match-token", SpokeInstanceUID: spokeUID, ProjectID: &project.ID,
			Capabilities: "pull", Actor: "member",
		})
		require.NoError(t, err)
		assert.Nil(t, rotated.Enrollment.RevokedAt)
		assert.Equal(t, 1, enrollmentCountByToken(t, rotated.Token))
	})

	t.Run("rotate retries a lost response exactly", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "rotation-retry-project")
		require.NoError(t, err)
		spokeUID := newSpokeUID(t)
		old := createEnrollment(t, "rotation-retry-old-token", spokeUID, &project.ID, "member")
		params := db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
			Token: "rotation-retry-replacement-token", SpokeInstanceUID: spokeUID, ProjectID: &project.ID,
			Capabilities: "push,pull", Actor: "member",
		}

		first, err := store.RotateFederationEnrollment(ctx, params)
		require.NoError(t, err)
		laterGrant := createEnrollment(
			t, "rotation-retry-later-token", spokeUID, &project.ID, "other-member",
		)
		replayed, err := store.RotateFederationEnrollment(ctx, params)
		require.NoError(t, err)

		assert.Equal(t, first.Enrollment.ID, replayed.Enrollment.ID)
		assert.Equal(t, first.Token, replayed.Token)
		assert.Equal(t, 1, enrollmentCountByToken(t, params.Token))
		assert.NotNil(t, enrollmentByID(t, old.Enrollment.ID).RevokedAt)
		assert.NotNil(t, enrollmentByID(t, laterGrant.Enrollment.ID).RevokedAt)
		assert.Nil(t, enrollmentByID(t, replayed.Enrollment.ID).RevokedAt)
	})

	t.Run("rotate rejects a mismatched replacement before revocation", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "rotation-conflict-project")
		require.NoError(t, err)
		spokeUID := newSpokeUID(t)
		params := db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
			Token: "rotation-conflict-replacement-token", SpokeInstanceUID: spokeUID, ProjectID: &project.ID,
			Capabilities: "pull", Actor: "member",
		}
		_, err = store.RotateFederationEnrollment(ctx, params)
		require.NoError(t, err)
		activeBeforeConflict := createEnrollment(
			t, "rotation-conflict-active-token", spokeUID, &project.ID, "member",
		)

		params.Actor = "different-member"
		_, err = store.RotateFederationEnrollment(ctx, params)
		assert.ErrorIs(t, err, db.ErrFederationEnrollmentTokenConflict)
		assert.Nil(t, enrollmentByID(t, activeBeforeConflict.Enrollment.ID).RevokedAt)
		assert.Equal(t, 1, enrollmentCountByToken(t, params.Token))
	})

	t.Run("concurrent exact rotations return one replacement", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "rotation-concurrent-exact-project")
		require.NoError(t, err)
		spokeUID := newSpokeUID(t)
		old := createEnrollment(
			t, "rotation-concurrent-exact-old-token", spokeUID, &project.ID, "member",
		)
		params := db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
			Token: "rotation-concurrent-exact-replacement-token", SpokeInstanceUID: spokeUID,
			ProjectID: &project.ID, Capabilities: "push,pull", Actor: "member",
		}
		results := runConcurrentFederationRotations(
			t, store, backend,
			[]db.CreateFederationEnrollmentParams{params, params},
		)
		first := results[0]
		second := results[1]
		require.NoError(t, first.err)
		require.NoError(t, second.err)
		assert.Equal(t, first.created.Enrollment.ID, second.created.Enrollment.ID)
		assert.Equal(t, params.Token, first.created.Token)
		assert.Equal(t, params.Token, second.created.Token)
		assert.Equal(t, 1, enrollmentCountByToken(t, params.Token))
		assert.NotNil(t, enrollmentByID(t, old.Enrollment.ID).RevokedAt)
		assert.Nil(t, enrollmentByID(t, first.created.Enrollment.ID).RevokedAt)
	})

	t.Run("concurrent conflicting rotations leave one active replacement", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "rotation-concurrent-conflict-project")
		require.NoError(t, err)
		spokeUID := newSpokeUID(t)
		old := createEnrollment(
			t, "rotation-concurrent-conflict-old-token", spokeUID, &project.ID, "member",
		)
		const replacementToken = "rotation-concurrent-conflict-replacement-token"
		params := []db.CreateFederationEnrollmentParams{
			{
				Token: replacementToken, SpokeInstanceUID: spokeUID, ProjectID: &project.ID,
				Capabilities: "pull", Actor: "member-a",
			},
			{
				Token: replacementToken, SpokeInstanceUID: spokeUID, ProjectID: &project.ID,
				Capabilities: "pull", Actor: "member-b",
			},
		}
		results := runConcurrentFederationRotations(t, store, backend, params)
		first := results[0]
		second := results[1]
		successes := []federationRotationResult{}
		conflicts := 0
		for _, result := range []federationRotationResult{first, second} {
			switch {
			case result.err == nil:
				successes = append(successes, result)
			case errors.Is(result.err, db.ErrFederationEnrollmentTokenConflict):
				conflicts++
			default:
				require.NoError(t, result.err)
			}
		}
		require.Len(t, successes, 1)
		assert.Equal(t, 1, conflicts)
		assert.Equal(t, replacementToken, successes[0].created.Token)
		assert.Equal(t, 1, enrollmentCountByToken(t, replacementToken))
		assert.NotNil(t, enrollmentByID(t, old.Enrollment.ID).RevokedAt)

		enrollments, listErr := store.ListFederationEnrollments(ctx)
		require.NoError(t, listErr)
		activeForScope := 0
		for _, enrollment := range enrollments {
			if enrollment.SpokeInstanceUID == spokeUID &&
				enrollment.ProjectID != nil &&
				*enrollment.ProjectID == project.ID &&
				enrollment.RevokedAt == nil {
				activeForScope++
				assert.Equal(t, successes[0].created.Enrollment.ID, enrollment.ID)
			}
		}
		assert.Equal(t, 1, activeForScope)
	})

	t.Run("rotate rolls back revocation when replacement insert fails", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "rotation-rollback-project")
		require.NoError(t, err)
		spokeUID := newSpokeUID(t)
		old := createEnrollment(t, "rotation-rollback-old-token", spokeUID, &project.ID, "member")
		removeFailure, err := backend.InstallEnrollmentInsertFailure(ctx, store)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, removeFailure()) })

		params := db.CreateFederationEnrollmentParams{
			Token: "rotation-rollback-replacement-token", SpokeInstanceUID: spokeUID, ProjectID: &project.ID,
			Capabilities: "pull", Actor: "member",
		}
		_, err = store.RotateFederationEnrollment(ctx, params)
		require.Error(t, err)
		require.NoError(t, removeFailure())

		assert.Nil(t, enrollmentByID(t, old.Enrollment.ID).RevokedAt)
		assert.Equal(t, 0, enrollmentCountByToken(t, params.Token))
	})

	return nil
}

func checkFederationEventTransport(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "federation-event-transport")
	if err != nil {
		return err
	}
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "local transport event", Author: "local-agent",
	})
	if err != nil {
		return err
	}
	_, commented, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "local-agent", Body: "portable comment",
	})
	if err != nil {
		return err
	}

	remote := newRemoteEvent(t, project, &issue.UID, "issue.updated", "remote-agent",
		"01HZNQ7VFPK1XGD8R5MABCD4EF", 100,
		json.RawMessage(`{"title":"remote title","updated_at":"2026-05-23T12:00:00.000Z"}`))
	remote.ProjectName = "remote-project-name"
	remote.ContentHash = remoteEventHash(t, remote)
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, remote)
	if err != nil {
		return fmt.Errorf("insert remote event: %w", err)
	}
	assert.True(t, inserted)
	stored, err := store.EventsByUIDs(ctx, project.ID, []string{remote.EventUID})
	if err != nil {
		return err
	}
	require.Len(t, stored, 1)
	assert.Equal(t, remote.EventUID, stored[0].UID)
	assert.Equal(t, remote.OriginInstanceUID, stored[0].OriginInstanceUID)
	assert.Equal(t, remote.ProjectName, stored[0].ProjectName)
	assert.Equal(t, remote.IssueUID, stored[0].IssueUID)
	assert.Nil(t, stored[0].IssueID)
	assert.Equal(t, remote.Actor, stored[0].Actor)
	assert.JSONEq(t, string(remote.Payload), stored[0].Payload)
	assert.Equal(t, remote.HLCPhysicalMS, stored[0].HLCPhysicalMS)
	assert.Equal(t, remote.HLCCounter, stored[0].HLCCounter)
	assert.Equal(t, remote.ContentHash, stored[0].ContentHash)
	assert.Equal(t, remote.CreatedAt, stored[0].CreatedAt)

	inserted, err = store.InsertRemoteEvent(ctx, project.ID, remote)
	if err != nil {
		return err
	}
	assert.False(t, inserted)
	conflict := remote
	conflict.Actor = "different-remote-agent"
	conflict.ContentHash = remoteEventHash(t, conflict)
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, conflict)
	assert.False(t, inserted)
	assert.ErrorIs(t, err, db.ErrRemoteEventConflict)
	badHash := newRemoteEvent(t, project, &issue.UID, "issue.updated", "remote-agent",
		remote.OriginInstanceUID, 101, json.RawMessage(`{"title":"bad hash"}`))
	badHash.ContentHash = strings.Repeat("0", 64)
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, badHash)
	assert.False(t, inserted)
	assert.ErrorIs(t, err, db.ErrRemoteEventHashMismatch)

	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), 0, 1)
	if err != nil {
		return err
	}
	require.Len(t, pending, 1)
	assert.Equal(t, created.ID, pending[0].ID)
	pending, err = store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), created.ID, 10)
	if err != nil {
		return err
	}
	require.Len(t, pending, 1)
	assert.Equal(t, commented.ID, pending[0].ID)
	count, highWater, err := store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), 0)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(2), count)
	assert.Equal(t, commented.ID, highWater)
	count, highWater, err = store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), commented.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, count)
	assert.Zero(t, highWater)

	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://push.example", HubProjectID: 42, HubProjectUID: project.UID,
		PushEnabled: true, Actor: "bound-agent", Enabled: true,
	})
	if err != nil {
		return err
	}
	boundIssue, boundCreated, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "bound issue actor", Author: "requesting-agent",
	})
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", boundIssue.Author)
	assert.Equal(t, "bound-agent", boundCreated.Actor)
	var createPayload struct {
		Author string `json:"author"`
	}
	require.NoError(t, json.Unmarshal([]byte(boundCreated.Payload), &createPayload))
	assert.Equal(t, "bound-agent", createPayload.Author)
	boundComment, boundEvent, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: boundIssue.ID, Author: "requesting-agent", Body: "bound event actor",
	})
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", boundComment.Author)
	assert.Equal(t, "bound-agent", boundEvent.Actor)
	var commentPayload struct {
		Author string `json:"author"`
	}
	require.NoError(t, json.Unmarshal([]byte(boundEvent.Payload), &commentPayload))
	assert.Equal(t, "bound-agent", commentPayload.Author)
	if _, _, err := db.ValidateRemoteEventContentHash(remoteEventFromStored(boundEvent)); err != nil {
		return fmt.Errorf("bound local event content hash: %w", err)
	}
	pending, err = store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), commented.ID, 10)
	if err != nil {
		return err
	}
	require.Len(t, pending, 2)
	assert.Equal(t, []int64{boundCreated.ID, boundEvent.ID}, []int64{pending[0].ID, pending[1].ID})
	assert.Equal(t, "bound-agent", pending[0].Actor)
	assert.Equal(t, "bound-agent", pending[1].Actor)

	hubStore := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hubStore.Close()) })
	hubProject, err := hubStore.CreateProjectWithUID(ctx, project.Name, project.UID)
	if err != nil {
		return err
	}
	if _, err := hubStore.EnableProjectFederation(ctx, hubProject.ID, "operator"); err != nil {
		return err
	}
	ingested, err := hubStore.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: store.InstanceUID(), BoundActor: "bound-agent",
		Events: []db.FederationIngestEvent{
			{SourceEventID: boundCreated.ID, Event: remoteEventFromStored(boundCreated)},
			{SourceEventID: boundEvent.ID, Event: remoteEventFromStored(boundEvent)},
		},
	})
	if err != nil {
		return fmt.Errorf("ingest bound create and comment: %w", err)
	}
	assert.Equal(t, 2, ingested.Accepted)
	hubIssue, err := hubStore.IssueByUID(ctx, boundIssue.UID, db.IncludeDeletedNo)
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", hubIssue.Author)
	hubComments, err := hubStore.CommentsByIssue(ctx, hubIssue.ID)
	if err != nil {
		return err
	}
	require.Len(t, hubComments, 1)
	assert.Equal(t, "bound-agent", hubComments[0].Author)

	label, labelEvent, err := store.AddLabelAndEvent(ctx, boundIssue.ID, db.LabelEventParams{
		Label: "triaged", EventType: "issue.labeled", Actor: "requesting-agent",
	})
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", label.Author)
	assert.Equal(t, "bound-agent", labelEvent.Actor)
	storedLabel, err := store.LabelByEndpoints(ctx, boundIssue.ID, "triaged")
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", storedLabel.Author)

	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "bound relationship peer", Author: "requesting-agent",
	})
	if err != nil {
		return err
	}
	link, linkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: boundIssue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "requesting-agent",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: boundIssue.ID,
		FromShortID: boundIssue.ShortID, FromUID: boundIssue.UID,
		ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "requesting-agent",
	})
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", link.Author)
	assert.Equal(t, "bound-agent", linkEvent.Actor)
	storedLink, err := store.LinkByEndpoints(ctx, boundIssue.ID, peer.ID, "blocks")
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", storedLink.Author)

	atomicPeer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "bound atomic peer", Author: "requesting-agent",
	})
	if err != nil {
		return err
	}
	atomicResult, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: boundIssue.ID, Actor: "requesting-agent", AddRelated: []int64{atomicPeer.ID},
	})
	if err != nil {
		return err
	}
	atomicLink, err := store.LinkByEndpoints(ctx, boundIssue.ID, atomicPeer.ID, "related")
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", atomicLink.Author)
	require.Len(t, atomicResult.Events, 1)
	assert.Equal(t, "bound-agent", atomicResult.Events[0].Actor)

	localEcho := remoteEventFromStored(created)
	found, err := store.ReconcileLocalFederationEcho(ctx, project.ID, localEcho)
	if err != nil {
		return err
	}
	assert.True(t, found)
	conflictingEcho := localEcho
	conflictingEcho.Actor = "unexpected-actor"
	conflictingEcho.ContentHash = remoteEventHash(t, conflictingEcho)
	found, err = store.ReconcileLocalFederationEcho(ctx, project.ID, conflictingEcho)
	assert.True(t, found)
	assert.ErrorIs(t, err, db.ErrRemoteEventConflict)
	missingEcho := newRemoteEvent(t, project, &issue.UID, "issue.updated", "local-agent",
		store.InstanceUID(), 102, json.RawMessage(`{"title":"missing"}`))
	found, err = store.ReconcileLocalFederationEcho(ctx, project.ID, missingEcho)
	if err != nil {
		return err
	}
	assert.False(t, found)
	return nil
}

func newRemoteEvent(
	t *testing.T,
	project db.Project,
	issueUID *string,
	eventType string,
	actor string,
	originInstanceUID string,
	physicalMS int64,
	payload json.RawMessage,
) db.RemoteEvent {
	t.Helper()
	eventUID, err := uid.New()
	require.NoError(t, err)
	event := db.RemoteEvent{
		EventUID: eventUID, OriginInstanceUID: originInstanceUID,
		ProjectUID: project.UID, ProjectName: project.Name, IssueUID: issueUID,
		Type: eventType, Actor: actor, HLCPhysicalMS: physicalMS, HLCCounter: 2,
		Payload: payload, CreatedAt: time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}
	event.ContentHash = remoteEventHash(t, event)
	return event
}

func remoteEventFromStored(event db.Event) db.RemoteEvent {
	return db.RemoteEvent{
		EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter, ContentHash: event.ContentHash,
		Payload: json.RawMessage(event.Payload), CreatedAt: event.CreatedAt,
	}
}

func remoteEventHash(t *testing.T, event db.RemoteEvent) string {
	t.Helper()
	hash, err := db.EventContentHash(db.EventHashInput{
		UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter,
		CreatedAt:  event.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Payload:    event.Payload,
	})
	require.NoError(t, err)
	return hash
}

func checkFederationResetLifecycle(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "federation-reset-project")
	if err != nil {
		return err
	}
	otherProject, err := store.CreateProject(ctx, "federation-reset-other")
	if err != nil {
		return err
	}
	issue, issueEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "reset projection", Author: "worker",
	})
	if err != nil {
		return err
	}
	otherIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: otherProject.ID, Title: "preserved projection", Author: "worker",
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
	principal := db.ClaimPrincipal{HolderInstanceUID: firstInstance, Holder: "holder", ClientKind: "agent"}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID, Principal: principal, ClaimKind: "hard",
	}); err != nil {
		return err
	}
	if _, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: issue.UID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: secondInstance, Holder: "waiting", ClientKind: "cli"},
		ClaimKind: "hard",
	}); err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://reset-hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		ReplayHorizonEventID: 9, PullCursorEventID: 8, PushEnabled: true,
		Actor: "bound-agent", Enabled: true,
	})
	if err != nil {
		return err
	}
	err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 20, 19,
		store.InstanceUID(), 0)
	assert.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	if _, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes); err != nil {
		return fmt.Errorf("blocked reset removed issue: %w", err)
	}
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(9), binding.ReplayHorizonEventID)
	assert.Equal(t, int64(8), binding.PullCursorEventID)

	if err := store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 20, 19,
		store.InstanceUID(), issueEvent.ID); err != nil {
		return fmt.Errorf("reset acknowledged projection: %w", err)
	}
	_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)
	if _, err := store.IssueByUID(ctx, otherIssue.UID, db.IncludeDeletedYes); err != nil {
		return fmt.Errorf("reset removed other project issue: %w", err)
	}
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
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	if err != nil {
		return err
	}
	assert.Empty(t, events)
	binding, err = store.FederationBindingByProject(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(20), binding.ReplayHorizonEventID)
	assert.Equal(t, int64(19), binding.PullCursorEventID)

	quarantineProject, err := store.CreateProject(ctx, "federation-reset-quarantine")
	if err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: quarantineProject.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://reset-hub.example", HubProjectID: 43, HubProjectUID: quarantineProject.UID,
		PushEnabled: true, Actor: "bound-agent", Enabled: true,
	})
	if err != nil {
		return err
	}
	_, err = store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: quarantineProject.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: 7, LastEventID: 9, EventUIDs: []string{"event-7"}, Error: "poisoned batch",
		CreatedAt: time.Date(2026, 7, 15, 21, 0, 0, 0, time.UTC),
	})
	if err != nil {
		return err
	}
	err = store.ResetFederatedProjectIfNoPendingPush(ctx, quarantineProject.ID, 20, 19,
		store.InstanceUID(), 0)
	assert.ErrorIs(t, err, db.ErrFederationResetBlockedByQuarantine)

	unsupportedProject, err := store.CreateProject(ctx, "federation-reset-unsupported")
	if err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: unsupportedProject.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://reset-hub.example", HubProjectID: 44, HubProjectUID: unsupportedProject.UID,
		PushEnabled: true, Actor: "bound-agent", Enabled: true,
	})
	if err != nil {
		return err
	}
	if backend.SeedUnsupportedFederationEvent == nil {
		return errors.New("backend must seed unsupported federation event fixtures")
	}
	unsupportedEventUID, err := uid.New()
	if err != nil {
		return err
	}
	if err := backend.SeedUnsupportedFederationEvent(ctx, store, unsupportedProject, unsupportedEventUID); err != nil {
		return fmt.Errorf("seed unsupported federation event: %w", err)
	}
	if err := store.ResetFederatedProjectIfNoPendingPush(ctx, unsupportedProject.ID, 30, 29,
		store.InstanceUID(), 0); err != nil {
		return fmt.Errorf("reset projection with unsupported local event: %w", err)
	}
	events, err = store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: unsupportedProject.ID, Limit: 10})
	if err != nil {
		return err
	}
	assert.Empty(t, events)

	unconditionalProject, err := store.CreateProject(ctx, "federation-reset-unconditional")
	if err != nil {
		return err
	}
	unconditionalIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: unconditionalProject.ID, Title: "discarded local projection", Author: "worker",
	})
	if err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: unconditionalProject.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://reset-hub.example", HubProjectID: 45, HubProjectUID: unconditionalProject.UID,
		Enabled: true,
	})
	if err != nil {
		return err
	}
	if err := store.ResetFederatedProject(ctx, unconditionalProject.ID, 40, 39); err != nil {
		return err
	}
	_, err = store.IssueByUID(ctx, unconditionalIssue.UID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)

	missingBindingProject, err := store.CreateProject(ctx, "federation-reset-unbound")
	if err != nil {
		return err
	}
	preserved, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: missingBindingProject.ID, Title: "rollback on missing binding", Author: "worker",
	})
	if err != nil {
		return err
	}
	err = store.ResetFederatedProject(ctx, missingBindingProject.ID, 50, 49)
	assert.ErrorIs(t, err, db.ErrNotFound)
	if _, err := store.IssueByUID(ctx, preserved.UID, db.IncludeDeletedYes); err != nil {
		return fmt.Errorf("missing-binding reset failed to roll back: %w", err)
	}
	return nil
}

func checkFederationProjectionLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	hub, err := store.CreateProject(ctx, "federation-projection-hub")
	if err != nil {
		return err
	}
	hubIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hub.ID, Title: "baseline issue", Body: "baseline body",
		Author: "author", Labels: []string{"baseline"},
	})
	if err != nil {
		return err
	}
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: hubIssue.ID, Author: "reviewer", Body: "baseline comment",
	})
	if err != nil {
		return err
	}
	binding, err := store.EnableProjectFederation(ctx, hub.ID, "operator")
	if err != nil {
		return err
	}
	assert.Equal(t, db.FederationRoleHub, binding.Role)
	assert.True(t, binding.Enabled)
	assert.Positive(t, binding.ReplayHorizonEventID)
	assert.Equal(t, binding.ReplayHorizonEventID-1, binding.PullCursorEventID)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: hub.ID, Limit: 100})
	if err != nil {
		return err
	}
	var enableEvent, snapshotEvent db.Event
	for _, event := range events {
		switch event.ID {
		case binding.ReplayHorizonEventID:
			enableEvent = event
		case binding.ReplayHorizonEventID + 1:
			snapshotEvent = event
		}
	}
	assert.Equal(t, "project.federation_enabled", enableEvent.Type)
	assert.Equal(t, "operator", enableEvent.Actor)
	assert.Equal(t, "issue.snapshot", snapshotEvent.Type)
	assert.Equal(t, enableEvent.HLCPhysicalMS, snapshotEvent.HLCPhysicalMS)
	assert.Equal(t, enableEvent.HLCCounter, snapshotEvent.HLCCounter)
	var snapshot struct {
		UID      string   `json:"uid"`
		Labels   []string `json:"labels"`
		Comments []struct {
			UID    string `json:"comment_uid"`
			Author string `json:"author"`
			Body   string `json:"body"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal([]byte(snapshotEvent.Payload), &snapshot))
	assert.Equal(t, hubIssue.UID, snapshot.UID)
	assert.Equal(t, []string{"baseline"}, snapshot.Labels)
	require.Len(t, snapshot.Comments, 1)
	assert.Equal(t, comment.UID, snapshot.Comments[0].UID)
	assert.Equal(t, "reviewer", snapshot.Comments[0].Author)
	assert.Equal(t, "baseline comment", snapshot.Comments[0].Body)
	highWater, err := store.MaxEventID(ctx)
	if err != nil {
		return err
	}
	idempotent, err := store.EnableProjectFederation(ctx, hub.ID, "other-operator")
	if err != nil {
		return err
	}
	assert.Equal(t, binding.ReplayHorizonEventID, idempotent.ReplayHorizonEventID)
	unchangedHighWater, err := store.MaxEventID(ctx)
	if err != nil {
		return err
	}
	assert.Equal(t, highWater, unchangedHighWater)
	refreshed, changed, err := store.RefreshProjectFederationBaseline(ctx, hub.ID, "operator")
	if err != nil {
		return err
	}
	assert.True(t, changed)
	assert.Greater(t, refreshed.ReplayHorizonEventID, binding.ReplayHorizonEventID)
	assert.Equal(t, refreshed.ReplayHorizonEventID-1, refreshed.PullCursorEventID)

	standalone, err := store.CreateProject(ctx, "federation-projection-standalone")
	if err != nil {
		return err
	}
	_, changed, err = store.RefreshProjectFederationBaseline(ctx, standalone.ID, "operator")
	if err != nil {
		return err
	}
	assert.False(t, changed)
	left, err := store.LeaveFederationReplica(ctx, standalone.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, standalone.UID, left.ProjectUID)
	assert.Empty(t, left.Role)
	_, err = store.LeaveFederationReplica(ctx, hub.ID)
	assert.ErrorIs(t, err, db.ErrFederationNotSpoke)

	spokeUID, err := uid.New()
	if err != nil {
		return err
	}
	spoke, err := store.CreateProjectWithUID(ctx, "federation-projection-spoke", spokeUID)
	if err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: spoke.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://projection.example", HubProjectID: 42, HubProjectUID: spoke.UID,
		Enabled: true,
	})
	if err != nil {
		return err
	}
	projectEvent := newRemoteEvent(t, spoke, nil, "project.federation_enabled", "remote-agent",
		"01HZNQ7VFPK1XGD8R5MABCD4EF", 200,
		json.RawMessage(`{"project_uid":"`+spoke.UID+`","project_name":"remote-project","metadata":{"tier":"shared"}}`))
	inserted, err := store.InsertRemoteEvent(ctx, spoke.ID, projectEvent)
	if err != nil {
		return err
	}
	assert.True(t, inserted)
	remoteIssueUID, err := uid.New()
	if err != nil {
		return err
	}
	remoteCommentUID, err := uid.New()
	if err != nil {
		return err
	}
	snapshotPayload := json.RawMessage(`{"uid":"` + remoteIssueUID + `","title":"remote issue","body":"remote body","author":"remote-agent","status":"open","metadata":{"source":"federation"},"labels":["remote"],"comments":[{"comment_uid":"` + remoteCommentUID + `","author":"remote-reviewer","body":"remote comment","created_at":"2026-05-23T12:00:00.000Z"}],"created_at":"2026-05-23T12:00:00.000Z","updated_at":"2026-05-23T12:00:00.000Z"}`)
	snapshotRemote := newRemoteEvent(t, spoke, &remoteIssueUID, "issue.snapshot", "remote-agent",
		projectEvent.OriginInstanceUID, 201, snapshotPayload)
	inserted, err = store.InsertRemoteEvent(ctx, spoke.ID, snapshotRemote)
	if err != nil {
		return err
	}
	assert.True(t, inserted)
	if err := store.MaterializeFederatedProject(ctx, spoke.ID); err != nil {
		return err
	}
	materialized, err := store.IssueByUID(ctx, remoteIssueUID, db.IncludeDeletedYes)
	if err != nil {
		return err
	}
	assert.Equal(t, "remote issue", materialized.Title)
	assert.JSONEq(t, `{"source":"federation"}`, string(materialized.Metadata))
	labels, err := store.LabelsForIssue(ctx, materialized.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, []string{"remote"}, labels)
	comments, err := store.CommentsByIssue(ctx, materialized.ID)
	if err != nil {
		return err
	}
	require.Len(t, comments, 1)
	assert.Equal(t, remoteCommentUID, comments[0].UID)
	assert.Equal(t, "remote comment", comments[0].Body)
	projectAfterMaterialize, err := store.ProjectByID(ctx, spoke.ID)
	if err != nil {
		return err
	}
	assert.JSONEq(t, `{"tier":"shared"}`, string(projectAfterMaterialize.Metadata))
	if err := store.MaterializeFederatedProject(ctx, spoke.ID); err != nil {
		return err
	}
	idempotentMaterialized, err := store.IssueByUID(ctx, remoteIssueUID, db.IncludeDeletedYes)
	if err != nil {
		return err
	}
	assert.Equal(t, materialized.Revision, idempotentMaterialized.Revision)
	updatedRemote := newRemoteEvent(t, spoke, &remoteIssueUID, "issue.updated", "remote-agent",
		projectEvent.OriginInstanceUID, 202,
		json.RawMessage(`{"title":"remote issue revised","updated_at":"2026-05-23T12:01:00.000Z"}`))
	inserted, err = store.InsertRemoteEvent(ctx, spoke.ID, updatedRemote)
	if err != nil {
		return err
	}
	assert.True(t, inserted)
	if err := store.MaterializeFederatedProject(ctx, spoke.ID); err != nil {
		return err
	}
	revised, err := store.IssueByUID(ctx, remoteIssueUID, db.IncludeDeletedYes)
	if err != nil {
		return err
	}
	assert.Equal(t, "remote issue revised", revised.Title)
	assert.Greater(t, revised.Revision, materialized.Revision)
	skipped, changed, err := store.RefreshProjectFederationBaseline(ctx, spoke.ID, "operator")
	if err != nil {
		return err
	}
	assert.False(t, changed)
	assert.Equal(t, db.FederationRoleSpoke, skipped.Role)

	principalUID, err := uid.New()
	if err != nil {
		return err
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: spoke.ID, IssueRef: remoteIssueUID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: principalUID, Holder: "worker", ClientKind: "agent"},
		ClaimKind: "hard",
	}); err != nil {
		return err
	}
	left, err = store.LeaveFederationReplica(ctx, spoke.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, db.FederationRoleSpoke, left.Role)
	assert.Equal(t, spoke.UID, left.ProjectUID)
	_, err = store.FederationBindingByProject(ctx, spoke.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	claimCount, err := store.CountLiveClaims(ctx, spoke.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, claimCount)
	if _, err := store.IssueByUID(ctx, remoteIssueUID, db.IncludeDeletedYes); err != nil {
		return fmt.Errorf("leave removed materialized issue: %w", err)
	}
	return nil
}

func checkFederationIngestLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	hub, err := store.CreateProject(ctx, "federation-ingest-hub")
	if err != nil {
		return err
	}
	if _, err := store.EnableProjectFederation(ctx, hub.ID, "operator"); err != nil {
		return err
	}
	spokeUID, err := uid.New()
	if err != nil {
		return err
	}
	issueUID, err := uid.New()
	if err != nil {
		return err
	}
	created := newRemoteEvent(t, hub, &issueUID, "issue.created", "sync-agent", spokeUID, 300,
		json.RawMessage(`{"uid":"`+issueUID+`","title":"pushed issue","body":"from spoke","author":"sync-agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	commentUID, err := uid.New()
	if err != nil {
		return err
	}
	commented := newRemoteEvent(t, hub, &issueUID, "issue.commented", "sync-agent", spokeUID, 301,
		json.RawMessage(`{"comment_uid":"`+commentUID+`","author":"sync-agent","body":"pushed comment","created_at":"2026-05-23T12:01:00.000Z"}`))
	params := db.FederationIngestParams{
		ProjectID: hub.ID, SpokeInstanceUID: spokeUID, BoundActor: "sync-agent",
		Events: []db.FederationIngestEvent{
			{SourceEventID: 10, Event: created},
			{SourceEventID: 11, Event: commented},
		},
	}
	result, err := store.IngestFederationEvents(ctx, params)
	if err != nil {
		return err
	}
	assert.Equal(t, 2, result.Accepted)
	assert.Zero(t, result.Duplicates)
	assert.Equal(t, int64(11), result.PushCursorEventID)
	assert.Equal(t, []string{created.EventUID, commented.EventUID}, result.InsertedEventUIDs)
	materialized, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	if err != nil {
		return err
	}
	assert.Equal(t, "pushed issue", materialized.Title)
	comments, err := store.CommentsByIssue(ctx, materialized.ID)
	if err != nil {
		return err
	}
	require.Len(t, comments, 1)
	assert.Equal(t, commentUID, comments[0].UID)
	assert.Equal(t, "pushed comment", comments[0].Body)

	retry, err := store.IngestFederationEvents(ctx, params)
	if err != nil {
		return err
	}
	assert.Zero(t, retry.Accepted)
	assert.Equal(t, 2, retry.Duplicates)
	assert.Equal(t, int64(11), retry.PushCursorEventID)
	assert.Empty(t, retry.InsertedEventUIDs)

	claimHolderUID, err := uid.New()
	if err != nil {
		return err
	}
	_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: hub.ID, IssueRef: issueUID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: claimHolderUID, Holder: "other-worker", ClientKind: "agent",
		},
		ClaimKind: "hard",
	})
	if err != nil {
		return err
	}
	updated := newRemoteEvent(t, hub, &issueUID, "issue.updated", "sync-agent", spokeUID, 302,
		json.RawMessage(`{"title":"uncovered update","updated_at":"2026-05-23T12:02:00.000Z"}`))
	audited, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hub.ID, SpokeInstanceUID: spokeUID, BoundActor: "sync-agent",
		Events: []db.FederationIngestEvent{{SourceEventID: 12, Event: updated}},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, audited.Accepted)
	require.Len(t, audited.InsertedEventUIDs, 2)
	assert.Equal(t, updated.EventUID, audited.InsertedEventUIDs[0])
	violations, violationCount, err := store.UnresolvedClaimViolationsForIssue(
		ctx, hub.ID, issueUID, 10,
	)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(1), violationCount)
	require.Len(t, violations, 1)
	assert.Equal(t, updated.EventUID, violations[0].OffendingEventUID)
	assert.Equal(t, spokeUID, violations[0].OffendingOriginInstanceUID)

	rollbackIssueUID, err := uid.New()
	if err != nil {
		return err
	}
	rollbackCreated := newRemoteEvent(t, hub, &rollbackIssueUID, "issue.created", "sync-agent", spokeUID, 302,
		json.RawMessage(`{"uid":"`+rollbackIssueUID+`","title":"rolled back","body":"","author":"sync-agent","status":"open","metadata":{},"created_at":"2026-05-23T12:02:00.000Z"}`))
	badHash := newRemoteEvent(t, hub, &rollbackIssueUID, "issue.updated", "sync-agent", spokeUID, 303,
		json.RawMessage(`{"title":"invalid","updated_at":"2026-05-23T12:03:00.000Z"}`))
	badHash.ContentHash = strings.Repeat("0", 64)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hub.ID, SpokeInstanceUID: spokeUID, BoundActor: "sync-agent",
		Events: []db.FederationIngestEvent{
			{SourceEventID: 12, Event: rollbackCreated},
			{SourceEventID: 13, Event: badHash},
		},
	})
	assert.ErrorIs(t, err, db.ErrRemoteEventHashMismatch)
	_, err = store.IssueByUID(ctx, rollbackIssueUID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)
	stored, err := store.EventsByUIDs(ctx, hub.ID, []string{created.EventUID, commented.EventUID})
	if err != nil {
		return err
	}
	assert.Len(t, stored, 2)

	actorMismatch := newRemoteEvent(t, hub, &issueUID, "issue.updated", "other-agent", spokeUID, 304,
		json.RawMessage(`{"title":"wrong actor","updated_at":"2026-05-23T12:04:00.000Z"}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hub.ID, SpokeInstanceUID: spokeUID, BoundActor: "sync-agent",
		Events: []db.FederationIngestEvent{{SourceEventID: 14, Event: actorMismatch}},
	})
	assert.ErrorIs(t, err, db.ErrFederationIngestValidation)
	wrongOrigin := actorMismatch
	wrongOrigin.Actor = "sync-agent"
	wrongOrigin.OriginInstanceUID = store.InstanceUID()
	wrongOrigin.ContentHash = remoteEventHash(t, wrongOrigin)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hub.ID, SpokeInstanceUID: spokeUID, BoundActor: "sync-agent",
		Events: []db.FederationIngestEvent{{SourceEventID: 15, Event: wrongOrigin}},
	})
	assert.ErrorIs(t, err, db.ErrFederationIngestValidation)
	zero, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: hub.ID})
	if err != nil {
		return err
	}
	assert.Equal(t, db.FederationIngestResult{}, zero)
	return nil
}

func checkFederationAdoptionIngestLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "federation-adoption-ingest")
	if err != nil {
		return err
	}
	if _, err := store.EnableProjectFederation(ctx, project.ID, "operator"); err != nil {
		return err
	}
	spokeUID, err := uid.New()
	if err != nil {
		return err
	}
	created, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: spokeUID, ProjectID: &project.ID, Capabilities: "push",
		Actor: "adoption-agent", AllowAdoptionSnapshotAuthors: true,
	})
	if err != nil {
		return err
	}
	metadata := newRemoteEvent(t, project, nil, "project.metadata_updated", "adoption-agent",
		spokeUID, 400, json.RawMessage(`{"project_uid":"`+project.UID+`","metadata":{"adopted":true}}`))
	first, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: project.ID, FederationEnrollmentID: created.Enrollment.ID,
		SpokeInstanceUID: spokeUID, BoundActor: "adoption-agent",
		AllowSnapshotAuthorPreservation: true, AdoptionBaseline: db.FederationAdoptionBaselineOpen,
		AdoptionBaselineEndSourceEventID: 12,
		Events:                           []db.FederationIngestEvent{{SourceEventID: 10, Event: metadata}},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, first.Accepted)
	enrollment, err := federationEnrollmentByID(ctx, store, created.Enrollment.ID)
	if err != nil {
		return err
	}
	assert.True(t, enrollment.AdoptionBaselineOpen)
	assert.True(t, enrollment.AllowAdoptionSnapshotAuthors)
	assert.Equal(t, int64(11), enrollment.AdoptionBaselineNextSourceEventID)
	assert.Equal(t, int64(12), enrollment.AdoptionBaselineEndSourceEventID)

	firstIssueUID, err := uid.New()
	if err != nil {
		return err
	}
	firstSnapshot := newRemoteEvent(t, project, &firstIssueUID, "issue.snapshot", "adoption-agent",
		spokeUID, 400, json.RawMessage(`{"uid":"`+firstIssueUID+`","title":"historical first","body":"","author":"historical-author","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	second, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: project.ID, FederationEnrollmentID: created.Enrollment.ID,
		SpokeInstanceUID: spokeUID, BoundActor: "adoption-agent",
		AllowSnapshotAuthorPreservation: true, AdoptionBaseline: db.FederationAdoptionBaselineOpen,
		AdoptionBaselineEndSourceEventID: 12,
		Events:                           []db.FederationIngestEvent{{SourceEventID: 11, Event: firstSnapshot}},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, second.Accepted)
	firstIssue, err := store.IssueByUID(ctx, firstIssueUID, db.IncludeDeletedYes)
	if err != nil {
		return err
	}
	assert.Equal(t, "historical-author", firstIssue.Author)
	enrollment, err = federationEnrollmentByID(ctx, store, created.Enrollment.ID)
	if err != nil {
		return err
	}
	assert.True(t, enrollment.AdoptionBaselineOpen)
	assert.False(t, enrollment.AllowAdoptionSnapshotAuthors)
	assert.Equal(t, int64(12), enrollment.AdoptionBaselineNextSourceEventID)

	secondIssueUID, err := uid.New()
	if err != nil {
		return err
	}
	secondSnapshot := newRemoteEvent(t, project, &secondIssueUID, "issue.snapshot", "adoption-agent",
		spokeUID, 400, json.RawMessage(`{"uid":"`+secondIssueUID+`","title":"historical second","body":"","author":"another-historical-author","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	terminalParams := db.FederationIngestParams{
		ProjectID: project.ID, FederationEnrollmentID: created.Enrollment.ID,
		SpokeInstanceUID: spokeUID, BoundActor: "adoption-agent",
		AllowSnapshotAuthorPreservation: true, AdoptionBaseline: db.FederationAdoptionBaselineComplete,
		AdoptionBaselineEndSourceEventID: 12,
		Events:                           []db.FederationIngestEvent{{SourceEventID: 12, Event: secondSnapshot}},
	}
	terminal, err := store.IngestFederationEvents(ctx, terminalParams)
	if err != nil {
		return err
	}
	assert.Equal(t, 1, terminal.Accepted)
	secondIssue, err := store.IssueByUID(ctx, secondIssueUID, db.IncludeDeletedYes)
	if err != nil {
		return err
	}
	assert.Equal(t, "adoption-agent", secondIssue.Author)
	enrollment, err = federationEnrollmentByID(ctx, store, created.Enrollment.ID)
	if err != nil {
		return err
	}
	assert.False(t, enrollment.AdoptionBaselineOpen)
	assert.False(t, enrollment.AllowAdoptionSnapshotAuthors)
	assert.Zero(t, enrollment.AdoptionBaselineNextSourceEventID)
	assert.Zero(t, enrollment.AdoptionBaselineEndSourceEventID)
	retry, err := store.IngestFederationEvents(ctx, terminalParams)
	if err != nil {
		return err
	}
	assert.Zero(t, retry.Accepted)
	assert.Equal(t, 1, retry.Duplicates)
	return nil
}

func federationEnrollmentByID(
	ctx context.Context,
	store db.Storage,
	enrollmentID int64,
) (db.FederationEnrollment, error) {
	enrollments, err := store.ListFederationEnrollments(ctx)
	if err != nil {
		return db.FederationEnrollment{}, err
	}
	for _, enrollment := range enrollments {
		if enrollment.ID == enrollmentID {
			return enrollment, nil
		}
	}
	return db.FederationEnrollment{}, db.ErrNotFound
}

func checkFederationProjectAdoption(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "federation-adoption-project")
	if err != nil {
		return err
	}
	patched, err := store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: project.ID, Actor: "local-author",
		Patch: map[string]json.RawMessage{"team": json.RawMessage(`"shared"`)},
	})
	if err != nil {
		return err
	}
	project = patched.Project
	first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "historical first", Author: "alice",
		Labels: []string{"history"},
	})
	if err != nil {
		return err
	}
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: first.ID, Author: "bob", Body: "current historical comment",
	})
	if err != nil {
		return err
	}
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "historical second", Author: "carol",
	})
	if err != nil {
		return err
	}
	claimInstanceUID, err := uid.New()
	if err != nil {
		return err
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID, IssueRef: first.UID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: claimInstanceUID, Holder: "alice", ClientKind: "agent",
		},
		ClaimKind: "hard",
	}); err != nil {
		return err
	}
	pendingInstanceUID, err := uid.New()
	if err != nil {
		return err
	}
	if _, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: second.UID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: pendingInstanceUID, Holder: "carol", ClientKind: "cli",
		},
		ClaimKind: "hard", Purpose: "offline work",
	}); err != nil {
		return err
	}
	_, pushFloor, err := store.PendingFederationPushStats(ctx, project.ID, store.InstanceUID(), 0)
	if err != nil {
		return err
	}
	assert.Positive(t, pushFloor)
	hubProjectUID, err := uid.New()
	if err != nil {
		return err
	}
	params := db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: hubProjectUID, ReplayHorizonEventID: 50,
		Actor: "adoption-agent", AllowInsecure: true,
	}
	result, err := store.AdoptProjectIntoFederation(ctx, params)
	if err != nil {
		return err
	}
	assert.Equal(t, int64(2), result.AdoptionSnapshotCount)
	assert.Equal(t, hubProjectUID, result.Project.UID)
	assert.Equal(t, db.FederationRoleSpoke, result.Binding.Role)
	assert.Equal(t, "https://hub.example", result.Binding.HubURL)
	assert.Equal(t, int64(42), result.Binding.HubProjectID)
	assert.Equal(t, hubProjectUID, result.Binding.HubProjectUID)
	assert.Equal(t, int64(50), result.Binding.ReplayHorizonEventID)
	assert.Equal(t, int64(49), result.Binding.PullCursorEventID)
	assert.True(t, result.Binding.PushEnabled)
	assert.Equal(t, pushFloor, result.Binding.PushCursorEventID)
	assert.Equal(t, "adoption-agent", result.Binding.Actor)
	assert.True(t, result.Binding.AllowInsecure)
	assert.True(t, result.Binding.Enabled)
	storedProject, err := store.ProjectByID(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, hubProjectUID, storedProject.UID)
	assert.JSONEq(t, `{"team":"shared"}`, string(storedProject.Metadata))
	for _, issueUID := range []string{first.UID, second.UID} {
		issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		if err != nil {
			return err
		}
		assert.Equal(t, hubProjectUID, issue.ProjectUID)
	}
	liveClaims, err := store.CountLiveClaims(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, liveClaims)
	pendingClaims, err := store.CountPendingClaims(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, pendingClaims)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	if err != nil {
		return err
	}
	require.Len(t, events, 3)
	assert.Equal(t, []string{"project.metadata_updated", "issue.snapshot", "issue.snapshot"},
		[]string{events[0].Type, events[1].Type, events[2].Type})
	assert.Equal(t, events[0].HLCPhysicalMS, events[1].HLCPhysicalMS)
	assert.Equal(t, events[0].HLCCounter, events[1].HLCCounter)
	assert.Equal(t, events[1].HLCPhysicalMS, events[2].HLCPhysicalMS)
	assert.Equal(t, events[1].HLCCounter, events[2].HLCCounter)
	for _, event := range events {
		assert.Equal(t, "adoption-agent", event.Actor)
		assert.Equal(t, hubProjectUID, event.ProjectUID)
	}
	var firstSnapshot db.Event
	for _, event := range events[1:] {
		if event.IssueUID != nil && *event.IssueUID == first.UID {
			firstSnapshot = event
		}
	}
	require.NotZero(t, firstSnapshot.ID)
	payload := db.PayloadMap(json.RawMessage(firstSnapshot.Payload))
	author, ok := db.StringValue(payload["author"])
	assert.True(t, ok)
	assert.Equal(t, "alice", author)
	var comments []struct {
		CommentUID string `json:"comment_uid"`
		Author     string `json:"author"`
		Body       string `json:"body"`
	}
	require.NoError(t, json.Unmarshal(payload["comments"], &comments))
	require.Len(t, comments, 1)
	assert.Equal(t, comment.UID, comments[0].CommentUID)
	assert.Equal(t, "bob", comments[0].Author)
	assert.Equal(t, "current historical comment", comments[0].Body)

	repeated, err := store.AdoptProjectIntoFederation(ctx, params)
	if err != nil {
		return err
	}
	assert.Zero(t, repeated.AdoptionSnapshotCount)
	repeatedEvents, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	if err != nil {
		return err
	}
	assert.Len(t, repeatedEvents, 3)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, result.Binding, binding)

	archived, err := store.CreateProject(ctx, "federation-adoption-archived")
	if err != nil {
		return err
	}
	if _, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: archived.ID, Actor: "operator", Force: true,
	}); err != nil {
		return err
	}
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: archived.ID, HubURL: "https://hub.example", HubProjectID: 43,
		HubProjectUID: archived.UID, ReplayHorizonEventID: 1, Actor: "adoption-agent",
	})
	assert.Error(t, err)
	return nil
}
