package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkExternalRootBindings(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "example-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "External root", Author: "tester",
	})
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}

	receiveAfter := time.Date(2026, 8, 20, 1, 0, 0, 123900000, time.UTC)
	binding, bound, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-1",
		ExternalAccountKey: "opaque-account-key", Actor: "tester",
		ReceiveCommentsAfter: receiveAfter,
	})
	if err != nil {
		return fmt.Errorf("create external root binding: %w", err)
	}
	assert.Equal(t, "issue.external_root_bound", bound.Type)
	assert.Equal(t, issue.ID, *bound.IssueID)
	assert.True(t, binding.Active)
	assert.True(t, binding.Enabled)
	assert.True(t, binding.ReceiveComments)
	assert.False(t, binding.PublishComments)
	assert.True(t, binding.CompleteExternal)
	assert.Equal(t, &receiveAfter, binding.ReceiveCommentsAfter)
	assert.NotEmpty(t, binding.UID)
	assert.NotZero(t, binding.RootMappingID)
	assert.NotContains(t, bound.Payload, binding.ExternalAccountKey)
	assert.NotContains(t, bound.Payload, "credential")
	assert.NotContains(t, bound.Payload, "config")
	commentTarget := int64(1)
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "connector:notes", ExternalID: "root-1", ObjectType: "issue",
		ProjectID: project.ID, IssueID: &issue.ID, CommentID: &commentTarget,
	})
	assert.ErrorIs(t, err, db.ErrExternalRootValidation)

	byIssue, err := store.ExternalRootBindingByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, binding.ID, byIssue.ID)
	byID, err := store.ExternalRootBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	assert.Equal(t, binding.UID, byID.UID)
	byRoot, err := store.ExternalRootBindingByExternalKey(ctx, "notes", "root-1")
	require.NoError(t, err)
	assert.Equal(t, binding.ID, byRoot.ID)

	// The root mapping and binding must commit together. The second mapping is
	// inserted before the active-issue constraint refuses the binding; the
	// transaction must roll that mapping back.
	_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-rollback",
		ExternalAccountKey: "opaque-account-key", Actor: "tester",
		ReceiveCommentsAfter: receiveAfter,
	})
	assert.ErrorIs(t, err, db.ErrExternalRootIssueAlreadyBound)
	_, err = store.ImportMappingBySource(ctx, project.ID, "connector:notes", "issue", "root-rollback")
	assert.ErrorIs(t, err, db.ErrNotFound)

	otherProject, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	otherIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: otherProject.ID, Title: "Other root", Author: "tester",
	})
	require.NoError(t, err)
	_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: otherProject.ID, IssueID: otherIssue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-1",
		ExternalAccountKey: "opaque-account-key", Actor: "tester",
		ReceiveCommentsAfter: receiveAfter,
	})
	assert.ErrorIs(t, err, db.ErrExternalRootAlreadyBound)
	_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: otherProject.ID, IssueID: otherIssue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-account-mismatch",
		ExternalAccountKey: "alternate-account", Actor: "tester",
		ReceiveCommentsAfter: receiveAfter,
	})
	assert.ErrorIs(t, err, db.ErrExternalRootValidation)
	_, err = store.ImportMappingBySource(ctx, otherProject.ID, "connector:notes", "issue", "root-account-mismatch")
	assert.ErrorIs(t, err, db.ErrNotFound)

	started := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	due, err := store.ListDueExternalRootBindings(ctx, started, started.Add(-db.ExternalRootClaimStaleAfter), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, binding.ID, due[0].ID)

	claimed, ok, err := store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-a", started, started.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "claim-a", claimed.ClaimToken)
	assert.Equal(t, &started, claimed.ClaimStartedAt)

	otherBinding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: otherProject.ID, IssueID: otherIssue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-2",
		ExternalAccountKey: "opaque-account-key", Actor: "tester",
		ReceiveCommentsAfter: receiveAfter,
	})
	require.NoError(t, err)
	due, err = store.ListDueExternalRootBindings(ctx, started.Add(time.Minute), started.Add(-4*time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, otherBinding.ID, due[0].ID, "a fresh claim must not consume the due-scan limit")
	_, _, err = store.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: otherBinding.ID, Actor: "tester", Reason: "fixture_cleanup",
	})
	require.NoError(t, err)

	_, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-b", started.Add(time.Minute), started.Add(-4*time.Minute),
	)
	require.NoError(t, err)
	assert.False(t, ok)

	takeoverAt := started.Add(10 * time.Minute)
	claimed, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-b", takeoverAt, started.Add(5*time.Minute),
	)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "claim-b", claimed.ClaimToken)
	assert.Equal(t, &takeoverAt, claimed.ClaimStartedAt)
	renewAt := takeoverAt.Add(4 * time.Minute)
	_, err = store.RenewExternalRootClaim(ctx, binding.ID, "wrong-claim", renewAt)
	assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
	renewed, renewErr := store.RenewExternalRootClaim(ctx, binding.ID, "claim-b", renewAt)
	require.NoError(t, renewErr)
	assert.Equal(t, "claim-b", renewed.ClaimToken)
	assert.Equal(t, &renewAt, renewed.ClaimStartedAt)
	_, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-after-renewal", takeoverAt.Add(6*time.Minute), takeoverAt.Add(time.Minute),
	)
	require.NoError(t, err)
	assert.False(t, ok, "a renewed live claim must not be stolen at the original expiry")

	_, ok, err = store.ClaimExternalRootBindingForManualReconcile(
		ctx, binding.ID, "manual-busy", takeoverAt, takeoverAt.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = store.ReleaseExternalRootClaim(ctx, binding.ID, "claim-a")
	assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)

	commentOne, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "tester", Body: "First comment",
	})
	require.NoError(t, err)
	commentTwo, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "tester", Body: "Second comment",
	})
	require.NoError(t, err)
	unrelatedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Unrelated root", Author: "tester",
	})
	require.NoError(t, err)
	unrelatedComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: unrelatedIssue.ID, Author: "tester", Body: "Unrelated comment",
	})
	require.NoError(t, err)
	issueID := issue.ID
	commentOneID := commentOne.ID
	commentMapping := db.ImportMappingParams{
		Source: db.ExternalRootPublishedCommentMappingSource(binding), ExternalID: "external-comment-1", ObjectType: "comment",
		ProjectID: project.ID, IssueID: &issueID, CommentID: &commentOneID,
	}
	unrelatedIssueID := unrelatedIssue.ID
	unrelatedCommentID := unrelatedComment.ID
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "connector:other", ExternalID: "external-unrelated", ObjectType: "comment",
		ProjectID: project.ID, IssueID: &unrelatedIssueID, CommentID: &unrelatedCommentID,
	})
	require.NoError(t, err)
	_, err = store.UpsertImportMapping(ctx, commentMapping)
	require.NoError(t, err)
	issueMappings, err := store.ImportCommentMappingsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, issueMappings, 1)
	assert.Equal(t, commentOne.ID, *issueMappings[0].CommentID)
	pendingAt := takeoverAt.Add(time.Minute)
	pending, err := store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: "claim-b", CommentUID: commentOne.UID, At: pendingAt,
	})
	require.NoError(t, err)
	assert.Equal(t, commentOne.UID, pending.PendingCommentUID)
	assert.Equal(t, &pendingAt, pending.PendingCommentStartedAt)
	_, err = store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: "claim-b", CommentUID: commentTwo.UID, At: pendingAt,
	})
	assert.ErrorIs(t, err, db.ErrExternalCommentPending)
	_, _, err = store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: "claim-a", CommentUID: commentOne.UID,
		ExpectedBody: commentOne.Body, Action: "published", Actor: "tester", At: pendingAt.Add(time.Minute), Mapping: &commentMapping,
		ExternalRevision: "published-comment-revision",
	})
	assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
	pending, resolved, err := store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: "claim-b", CommentUID: commentOne.UID,
		ExpectedBody: commentOne.Body, Action: "published", Actor: "tester", At: pendingAt.Add(time.Minute), Mapping: &commentMapping,
		ExternalRevision: "published-comment-revision",
	})
	require.NoError(t, err)
	assert.Empty(t, pending.PendingCommentUID)
	assert.Nil(t, pending.PendingCommentStartedAt)
	assert.Equal(t, "issue.external_comment_resolved", resolved.Type)

	mappingClock := time.Date(2026, 8, 20, 10, 0, 0, 123456789, time.UTC)
	restoreMappingClock := backend.InstallExternalRootClock(store, func() time.Time { return mappingClock })
	mappingOne, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "notes", KataField: "scheduled_on",
		ExternalFieldID: "start-1", ExternalFieldName: "Start",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-1",
	})
	require.NoError(t, err)
	assert.True(t, mappingOne.Active)
	mappingTwo, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "notes", KataField: "scheduled_on",
		ExternalFieldID: "start-2", ExternalFieldName: "Start",
		AcceptedKinds: []string{"date", "local_datetime"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-2",
	})
	require.NoError(t, err)
	restoreMappingClock()
	assert.NotEqual(t, mappingOne.ID, mappingTwo.ID)
	assert.True(t, mappingTwo.CreatedAt.After(mappingOne.CreatedAt),
		"mapping revisions created at the same clock instant need distinct portable identities")
	orderingStore := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, orderingStore.Close()) })
	earlier := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
	later := earlier.Add(900 * time.Millisecond)
	require.NoError(t, orderingStore.ImportReplay(ctx, []db.ImportRecord{
		{Kind: db.ImportKindExternalFieldMapping, ExternalFieldMapping: &db.ExternalFieldMappingExport{
			ConnectorInstance: "timestamp-order", KataField: "scheduled_on",
			ExternalFieldID: "start-earlier", ExternalFieldName: "Start earlier",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-earlier", CreatedAt: earlier, UpdatedAt: earlier,
		}},
		{Kind: db.ImportKindExternalFieldMapping, ExternalFieldMapping: &db.ExternalFieldMappingExport{
			ConnectorInstance: "timestamp-order", KataField: "scheduled_on",
			ExternalFieldID: "start-later", ExternalFieldName: "Start later",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-later", CreatedAt: later, UpdatedAt: later,
		}},
	}, db.ImportOptions{}))
	restoreOrderingClock := backend.InstallExternalRootClock(orderingStore, func() time.Time { return later })
	orderedMapping, err := orderingStore.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "timestamp-order", KataField: "scheduled_on",
		ExternalFieldID: "start-current", ExternalFieldName: "Start current",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-current",
	})
	restoreOrderingClock()
	require.NoError(t, err)
	assert.True(t, orderedMapping.CreatedAt.After(later),
		"mapping identity ordering must use time, not variable-width timestamp text")
	mappings, err := store.ListExternalFieldMappings(ctx, "notes")
	require.NoError(t, err)
	require.Len(t, mappings, 2)
	assert.False(t, mappings[0].Active)
	assert.True(t, mappings[1].Active)

	type mappingResult struct {
		mapping db.ExternalFieldMapping
		err     error
	}
	startMappingRace := make(chan struct{})
	mappingRaceResults := make(chan mappingResult, 2)
	var mappingRace sync.WaitGroup
	for range 2 {
		mappingRace.Go(func() {
			<-startMappingRace
			mapping, raceErr := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
				ConnectorInstance: "calendar", KataField: "scheduled_on",
				ExternalFieldID: "shared-date", ExternalFieldName: "Shared date",
				AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
				SchemaRevision: "schema-1",
			})
			mappingRaceResults <- mappingResult{mapping: mapping, err: raceErr}
		})
	}
	close(startMappingRace)
	mappingRace.Wait()
	close(mappingRaceResults)
	var racedMappings []db.ExternalFieldMapping
	for result := range mappingRaceResults {
		require.NoError(t, result.err)
		racedMappings = append(racedMappings, result.mapping)
	}
	require.Len(t, racedMappings, 2)
	assert.Equal(t, racedMappings[0].ID, racedMappings[1].ID)
	_, err = store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "notes", KataField: "deadline_on",
		ExternalFieldID: "due-invalid", ExternalFieldName: "Due",
		AcceptedKinds: []string{"date", "date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-2",
	})
	assert.ErrorIs(t, err, db.ErrExternalFieldMappingValidation)
	_, err = store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "notes", KataField: "deadline_on",
		ExternalFieldID: "due-invalid", ExternalFieldName: "Due",
		AcceptedKinds: []string{"date"}, Nullable: false, Writable: true,
		SchemaRevision: "schema-2",
	})
	assert.ErrorIs(t, err, db.ErrExternalFieldMappingValidation)

	deadlineMapping, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "notes", KataField: "deadline_on",
		ExternalFieldID: "due-1", ExternalFieldName: "Due",
		AcceptedKinds: []string{"instant"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-2",
	})
	require.NoError(t, err)
	stateAt := pendingAt.Add(2 * time.Minute)
	const fieldClaimToken = "claim-fields"
	_, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, fieldClaimToken, stateAt, stateAt.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	require.True(t, ok)
	deadlineState, event, err := store.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
		BindingID: binding.ID, MappingID: deadlineMapping.ID, ClaimToken: fieldClaimToken,
		Baseline: json.RawMessage(`"2026-08-21T12:00:00Z"`), At: stateAt, Actor: "tester",
	})
	require.NoError(t, err)
	assert.Nil(t, event)
	assert.False(t, deadlineState.Conflicted)

	conflictAt := stateAt.Add(time.Minute)
	conflicted, event, err := store.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
		BindingID: binding.ID, MappingID: mappingTwo.ID, ClaimToken: fieldClaimToken,
		Baseline:         json.RawMessage(`"2026-08-20"`),
		ConflictKata:     json.RawMessage(`"2026-08-21"`),
		ConflictExternal: json.RawMessage(`"2026-08-22"`),
		Conflicted:       true, At: conflictAt, Actor: "tester",
	})
	require.NoError(t, err)
	require.NotNil(t, event)
	assert.Equal(t, "issue.external_field_conflicted", event.Type)
	assert.True(t, conflicted.Conflicted)
	assert.Equal(t, &conflictAt, conflicted.ConflictAt)
	states, err := store.ExternalFieldStates(ctx, binding.ID)
	require.NoError(t, err)
	require.Len(t, states, 2)
	stateByMapping := make(map[int64]db.ExternalFieldState, len(states))
	for _, state := range states {
		stateByMapping[state.MappingID] = state
	}
	assert.True(t, stateByMapping[mappingTwo.ID].Conflicted)
	assert.False(t, stateByMapping[deadlineMapping.ID].Conflicted)

	resolvedState, fieldResolved, err := store.ResolveExternalFieldConflict(ctx, db.ResolveExternalFieldConflictParams{
		BindingID: binding.ID, MappingID: mappingTwo.ID, ClaimToken: fieldClaimToken,
		Baseline: json.RawMessage(`"2026-08-21"`), Actor: "tester", At: conflictAt.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.False(t, resolvedState.Conflicted)
	assert.Nil(t, resolvedState.ConflictAt)
	assert.Equal(t, "issue.external_field_resolved", fieldResolved.Type)
	unmapped, err := store.UnmapExternalField(ctx, "notes", "scheduled_on")
	require.NoError(t, err)
	assert.False(t, unmapped.Active)
	mappings, err = store.ListExternalFieldMappings(ctx, "notes")
	require.NoError(t, err)
	require.Len(t, mappings, 3)

	failureAt := conflictAt.Add(2 * time.Minute)
	_, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-b", failureAt, failureAt.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	require.True(t, ok)
	retryAt := failureAt.Add(5 * time.Minute)
	_, err = store.RecordExternalRootError(ctx, db.ExternalRootErrorParams{
		BindingID: binding.ID, ClaimToken: "claim-b", At: failureAt, NextAttemptAt: retryAt,
		Error: "request failed", ExternalState: "complete",
	})
	assert.ErrorIs(t, err, db.ErrExternalRootValidation)
	_, err = store.RecordExternalRootError(ctx, db.ExternalRootErrorParams{
		BindingID: binding.ID, ClaimToken: "claim-b", At: failureAt, NextAttemptAt: retryAt,
		Error: "request failed", ExternalRevision: "completed-revision",
	})
	assert.ErrorIs(t, err, db.ErrExternalRootValidation)
	failed, err := store.RecordExternalRootError(ctx, db.ExternalRootErrorParams{
		BindingID: binding.ID, ClaimToken: "claim-b", At: failureAt, NextAttemptAt: retryAt,
		Error:         "request failed: token=private-value " + strings.Repeat("x", 4096),
		ExternalState: "complete", ExternalRevision: "completed-revision",
	})
	require.NoError(t, err)
	assert.Empty(t, failed.ClaimToken)
	assert.Equal(t, 1, failed.ConsecutiveFailures)
	assert.Equal(t, &failureAt, failed.LastAttemptAt)
	assert.Equal(t, &failureAt, failed.LastErrorAt)
	assert.Equal(t, &retryAt, failed.NextAttemptAt)
	assert.Nil(t, failed.LastSuccessAt)
	assert.Equal(t, "complete", failed.LastExternalState)
	assert.Equal(t, "completed-revision", failed.LastExternalRevision)
	assert.NotContains(t, failed.LastError, "private-value")
	assert.LessOrEqual(t, len(failed.LastError), 2048)
	_, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-c", retryAt.Add(-time.Second), failureAt,
	)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = store.ClaimExternalRootBinding(ctx, binding.ID, "claim-c", retryAt, failureAt)
	require.NoError(t, err)
	assert.True(t, ok)
	nextPollAt := retryAt.Add(10 * time.Minute)
	success, err := store.RecordExternalRootSuccess(ctx, db.ExternalRootSuccessParams{
		BindingID: binding.ID, ClaimToken: "claim-c", At: retryAt.Add(time.Minute),
		NextAttemptAt: nextPollAt, ExternalState: "open", ExternalRevision: "revision-2",
	})
	require.NoError(t, err)
	assert.Empty(t, success.ClaimToken)
	assert.Zero(t, success.ConsecutiveFailures)
	assert.Empty(t, success.LastError)
	assert.Equal(t, "open", success.LastExternalState)
	assert.Equal(t, "revision-2", success.LastExternalRevision)
	assert.Equal(t, &nextPollAt, success.NextAttemptAt)
	_, err = store.RecordExternalRootSuccess(ctx, db.ExternalRootSuccessParams{
		BindingID: binding.ID, ClaimToken: "claim-c", At: nextPollAt,
		NextAttemptAt: nextPollAt.Add(time.Minute),
	})
	assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
	_, ok, err = store.ClaimExternalRootBinding(ctx, binding.ID, "claim-d", nextPollAt, retryAt)
	require.NoError(t, err)
	assert.True(t, ok)
	released, err := store.ReleaseExternalRootClaim(ctx, binding.ID, "claim-d")
	require.NoError(t, err)
	assert.Empty(t, released.ClaimToken)

	paused, pausedEvent, err := store.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: binding.ID, Actor: "tester", Reason: "operator_pause",
	})
	require.NoError(t, err)
	assert.False(t, paused.Enabled)
	assert.Equal(t, "operator_pause", paused.PausedReason)
	assert.Empty(t, paused.ClaimToken)
	assert.Equal(t, "issue.external_root_paused", pausedEvent.Type)
	due, err = store.ListDueExternalRootBindings(ctx, takeoverAt.Add(time.Hour), takeoverAt, 10)
	require.NoError(t, err)
	assert.Empty(t, due)
	_, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-c", takeoverAt.Add(time.Hour), takeoverAt,
	)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = store.ClaimExternalRootBindingForManualReconcile(
		ctx, binding.ID, "manual-paused", takeoverAt.Add(time.Hour), takeoverAt,
	)
	require.NoError(t, err)
	assert.False(t, ok)
	manualAction, ok, err := store.ClaimExternalRootBindingForManualAction(
		ctx, binding.ID, "action-paused", takeoverAt.Add(time.Hour), takeoverAt,
	)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, manualAction.Enabled)
	pausedRenewalAt := takeoverAt.Add(time.Hour + time.Minute)
	renewedPaused, err := store.RenewExternalRootClaim(ctx, binding.ID, "action-paused", pausedRenewalAt)
	require.NoError(t, err)
	assert.False(t, renewedPaused.Enabled)
	assert.Equal(t, &pausedRenewalAt, renewedPaused.ClaimStartedAt)
	_, err = store.ReleaseExternalRootClaim(ctx, binding.ID, "action-paused")
	require.NoError(t, err)

	resumed, resumedEvent, err := store.ResumeExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: binding.ID, Actor: "tester",
	})
	require.NoError(t, err)
	assert.True(t, resumed.Enabled)
	assert.Empty(t, resumed.PausedReason)
	assert.Nil(t, resumed.NextAttemptAt)
	assert.Equal(t, "issue.external_root_resumed", resumedEvent.Type)
	resumeAttempt := retryAt.Add(2 * time.Minute)
	_, ok, err = store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-after-resume", resumeAttempt, failureAt,
	)
	require.NoError(t, err)
	require.True(t, ok, "resume must make a backoff-scheduled binding immediately claimable")
	_, err = store.ReleaseExternalRootClaim(ctx, binding.ID, "claim-after-resume")
	require.NoError(t, err)
	unbound, unboundEvent, err := store.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: binding.ID, Actor: "tester",
	})
	require.NoError(t, err)
	assert.False(t, unbound.Active)
	assert.False(t, unbound.Enabled)
	assert.NotNil(t, unbound.UnboundAt)
	assert.Equal(t, "issue.external_root_unbound", unboundEvent.Type)
	_, err = store.ExternalRootBindingByIssue(ctx, issue.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	historical, err := store.ExternalRootBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	assert.False(t, historical.Active)
	_, ok, err = store.ClaimExternalRootBindingForManualAction(
		ctx, binding.ID, "inactive-action", takeoverAt.Add(2*time.Hour), takeoverAt,
	)
	require.NoError(t, err)
	assert.False(t, ok)
	rebound, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-1",
		ExternalAccountKey: "opaque-account-key", Actor: "tester",
		ReceiveCommentsAfter: receiveAfter.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.NotEqual(t, binding.ID, rebound.ID)
	assert.Equal(t, binding.RootMappingID, rebound.RootMappingID)

	// Bridge audit events federate as issue-scoped history while the receiver
	// deliberately retains no local binding state.
	pendingEvents, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), created.ID, 100)
	require.NoError(t, err)
	var bridgeEvents []db.Event
	for _, candidate := range pendingEvents {
		if strings.HasPrefix(candidate.Type, "issue.external_") {
			bridgeEvents = append(bridgeEvents, candidate)
		}
	}
	require.NotEmpty(t, bridgeEvents)

	hubStore := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hubStore.Close()) })
	hubProject, err := hubStore.CreateProjectWithUID(ctx, "example-project", project.UID)
	require.NoError(t, err)
	_, err = hubStore.EnableProjectFederation(ctx, hubProject.ID, "operator")
	require.NoError(t, err)
	transport := []db.FederationIngestEvent{{SourceEventID: created.ID, Event: remoteEventFromStored(created)}}
	for _, candidate := range bridgeEvents {
		transport = append(transport, db.FederationIngestEvent{
			SourceEventID: candidate.ID, Event: remoteEventFromStored(candidate),
		})
	}
	result, err := hubStore.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: store.InstanceUID(), BoundActor: "tester",
		Events: transport,
	})
	if err != nil {
		return fmt.Errorf("ingest external-root audit events: %w", err)
	}
	assert.Equal(t, len(transport), result.Accepted)
	hubIssue, err := hubStore.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, issue.Title, hubIssue.Title)
	assert.Equal(t, issue.Body, hubIssue.Body)
	assert.Equal(t, issue.Status, hubIssue.Status)
	_, err = hubStore.ExternalRootBindingByIssue(ctx, hubIssue.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	return nil
}
