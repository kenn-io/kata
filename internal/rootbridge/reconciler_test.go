package rootbridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/pkg/connector"
)

func TestReconcileInboundRootUsesNativeEventAndPreservesAttention(t *testing.T) {
	h := newReconcileHarness(t)
	metadataBefore := h.setAttention(t)
	h.client.read.Title = "External plan"
	h.client.read.Body = "External body"
	h.client.read.Revision = "root-revision-2"
	h.client.read.UpdatedAt = h.boundAt.Add(2 * time.Minute)
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	h.client.read.Actor = &connector.Actor{ID: "actor-4", DisplayName: "Reviewer"}

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.True(t, result.RootUpdated)

	issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "External plan", issue.Title)
	assert.Equal(t, "External body", issue.Body)
	assert.JSONEq(t, string(metadataBefore), string(issue.Metadata))

	event := requireEventType(t, result.Events, "issue.updated")
	assert.Equal(t, "connector:notes", event.Actor)
	var payload struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Source struct {
			ConnectorInstance string `json:"connector_instance"`
			ExternalRevision  string `json:"external_revision"`
			ActorID           string `json:"actor_id"`
			ActorName         string `json:"actor_name"`
			UpdatedAt         string `json:"updated_at"`
			ObservedAt        string `json:"observed_at"`
		} `json:"source"`
	}
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	assert.Equal(t, "External plan", payload.Title)
	assert.Equal(t, "External body", payload.Body)
	assert.Equal(t, "notes", payload.Source.ConnectorInstance)
	assert.Equal(t, "root-revision-2", payload.Source.ExternalRevision)
	assert.Equal(t, "actor-4", payload.Source.ActorID)
	assert.Equal(t, "Reviewer", payload.Source.ActorName)
	assert.Equal(t, h.client.read.UpdatedAt.Format(time.RFC3339Nano), payload.Source.UpdatedAt)
	assert.Equal(t, h.client.read.ObservedAt.Format(time.RFC3339Nano), payload.Source.ObservedAt)
	h.requireSuccessfulClaim(t, "open", "root-revision-2")
}

func TestReconcileRenewsClaimDuringLongConnectorCall(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	comment, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: h.issue.ID, Author: "tester", Body: "Wait for publication",
	})
	require.NoError(t, err)
	h.client.publishResult = publishedComment(h, "long-call-comment", comment.Body)
	publishedAt := time.Now().UTC().Add(time.Minute)
	h.client.publishResult.CreatedAt = publishedAt
	h.client.publishResult.UpdatedAt = publishedAt
	entered := make(chan struct{})
	release := make(chan struct{})
	h.client.beforePublish = func() {
		close(entered)
		<-release
	}
	const staleAfter = 120 * time.Millisecond
	h.reconciler = NewReconciler(h.store, h.registry, ReconcilerConfig{ClaimStaleAfter: staleAfter})
	type outcome struct {
		result RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := h.reconciler.Run(context.Background(), h.binding.ID, RunOptions{})
		done <- outcome{result: result, err: runErr}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "publication did not start")
	}
	time.Sleep(3 * staleAfter)
	now := time.Now().UTC()
	_, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, "competing-long-call", now, now.Add(-staleAfter),
	)
	require.NoError(t, err)
	assert.False(t, acquired, "a valid long connector call must retain its exclusive claim")
	close(release)
	completed := <-done
	require.NoError(t, completed.err)
	assert.Equal(t, 1, h.client.publishCalls)
}

func TestReconcileRejectsStaleRootBeforeContentOrLifecycleProjection(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.read.Title = "Current external plan"
	h.client.read.Body = "Current external body"
	h.client.read.State = "complete"
	h.client.read.Revision = "revision-current"
	h.client.read.UpdatedAt = h.boundAt.Add(2 * time.Minute)
	h.client.read.ObservedAt = h.boundAt.Add(3 * time.Minute)

	current, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, current.CompletionRequests)

	h.client.read.Title = "Stale external plan"
	h.client.read.Body = "Stale external body"
	h.client.read.State = "open"
	h.client.read.Revision = "revision-stale"
	h.client.read.UpdatedAt = h.boundAt.Add(time.Minute)
	h.client.read.ObservedAt = h.boundAt.Add(4 * time.Minute)

	stale, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.ErrorIs(t, err, db.ErrExternalRootValidation)
	assert.Zero(t, stale.RootUpdated)
	assert.Zero(t, stale.ReopenRequests)
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "Current external plan", issue.Title)
	assert.Equal(t, "Current external body", issue.Body)
	comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Len(t, comments, 1)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "complete", binding.LastExternalState)
	assert.Equal(t, "revision-current", binding.LastExternalRevision)
}

func TestCompleteExternalClosedKataUsesNativeStateWithoutPublishingComment(t *testing.T) {
	for _, reason := range []string{"done", "wontfix", "duplicate", "superseded", "audit-no-change"} {
		t.Run(reason, func(t *testing.T) {
			h := newReconcileHarness(t)
			_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, reason, "verifier", "", nil)
			require.NoError(t, err)
			h.client.completeReadback = completedRoot(h.client.read, "completed-by-kata")

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

			require.NoError(t, err)
			assert.Equal(t, 1, h.client.completeCalls)
			assert.Zero(t, h.client.publishCalls)
			assert.Empty(t, result.Events)
			comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.Empty(t, comments)
			h.requireSuccessfulClaim(t, "complete", "completed-by-kata")
		})
	}
}

func TestCompleteExternalAlreadyCompleteIsIdempotent(t *testing.T) {
	h := newReconcileHarness(t)
	_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.read = completedRoot(h.client.read, "already-complete")

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, h.client.completeCalls)
	h.requireSuccessfulClaim(t, "complete", "already-complete")
}

func TestCompleteExternalDisabledSuppressesConnectorCall(t *testing.T) {
	h := newReconcileHarness(t)
	closed, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	snapshot := reconcileSnapshot{
		binding: h.binding,
		issue:   closed,
		client:  h.client,
		root:    h.client.read,
	}
	snapshot.binding.CompleteExternal = false

	_, err = h.reconciler.completeExternal(t.Context(), &snapshot, "unused-claim", RunResult{})

	require.NoError(t, err)
	assert.Zero(t, h.client.completeCalls)
}

func TestCompleteExternalFencesReopenBeforeConnectorCall(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: h.issue.ID, Author: "tester", Body: "Published before reopen",
	})
	require.NoError(t, err)
	h.client.publishResult = publishedComment(h, "published-before-reopen", local.Body)
	_, _, _, err = h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.completeReadback = completedRoot(h.client.read, "must-not-complete")
	entered := make(chan struct{})
	release := make(chan struct{})
	h.reconciler.store = &afterPublishedCommentBarrierStorage{
		Storage: h.store, entered: entered, release: release,
	}
	type outcome struct {
		result RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := h.reconciler.Run(context.Background(), h.binding.ID, RunOptions{})
		done <- outcome{result: result, err: runErr}
	}()

	<-entered
	_, _, changed, reopenErr := h.store.ReopenIssue(t.Context(), h.issue.ID, "operator")
	close(release)
	require.ErrorIs(t, reopenErr, db.ErrExternalRootClaimActive)
	require.False(t, changed)
	got := <-done

	require.NoError(t, got.err)
	assert.Equal(t, 1, h.client.completeCalls)
	assert.Contains(t, eventTypes(got.result.Events), "issue.external_comment_resolved")
	_, _, changed, reopenErr = h.store.ReopenIssue(t.Context(), h.issue.ID, "operator")
	require.NoError(t, reopenErr)
	require.True(t, changed)
	stored, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "open", stored.Status)
}

func TestCompleteExternalRevalidatesClaimBeforeConnectorCall(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: h.issue.ID, Author: "tester", Body: "Published before pause",
	})
	require.NoError(t, err)
	h.client.publishResult = publishedComment(h, "published-before-pause", local.Body)
	_, _, _, err = h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.completeReadback = completedRoot(h.client.read, "must-not-complete")
	entered := make(chan struct{})
	release := make(chan struct{})
	h.reconciler.store = &afterPublishedCommentBarrierStorage{
		Storage: h.store, entered: entered, release: release,
	}
	type outcome struct {
		result RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := h.reconciler.Run(context.Background(), h.binding.ID, RunOptions{})
		done <- outcome{result: result, err: runErr}
	}()

	<-entered
	claimed, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	paused, _, pauseErr := h.store.PauseExternalRootBinding(t.Context(), db.ExternalRootActionParams{
		BindingID: h.binding.ID, ClaimToken: claimed.ClaimToken,
		Actor: "operator", Reason: "operator_pause",
	})
	close(release)
	require.NoError(t, pauseErr)
	got := <-done

	assert.ErrorIs(t, got.err, db.ErrExternalRootClaimLost)
	assert.Zero(t, h.client.completeCalls)
	assert.Contains(t, eventTypes(got.result.Events), "issue.external_comment_resolved")
	assert.True(t, paused.Active)
	assert.False(t, paused.Enabled)
	assert.Empty(t, paused.ClaimToken)
}

func TestCompleteExternalRevalidatesBindingIdentityBeforeConnectorCall(t *testing.T) {
	h := newReconcileHarness(t)
	_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.completeReadback = completedRoot(h.client.read, "must-not-complete")
	h.reconciler.store = &mutateCompletionBindingReadStorage{Storage: h.store}

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
	assert.Zero(t, h.client.completeCalls)
}

func TestCompleteExternalRevalidatesLiveDisableBeforeConnectorCall(t *testing.T) {
	h := newReconcileHarness(t)
	_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.completeReadback = completedRoot(h.client.read, "must-not-complete")
	h.reconciler.store = &mutateCompletionBindingReadStorage{
		Storage: h.store,
		mutate: func(binding *db.ExternalRootBinding) {
			binding.CompleteExternal = false
		},
	}

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, h.client.completeCalls)
}

func TestCompleteExternalRequiresVerifiedReadbackAndRetries(t *testing.T) {
	h := newReconcileHarness(t)
	_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.completeReadback = h.client.read

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.Error(t, err)
	assert.Equal(t, 1, h.client.completeCalls)
	closed, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "closed", closed.Status)
	failed, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, 1, failed.ConsecutiveFailures)
	assert.Empty(t, failed.ClaimToken)

	h.client.completeReadback = completedRoot(h.client.read, "completed-on-retry")
	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, h.client.completeCalls)
	h.requireSuccessfulClaim(t, "complete", "completed-on-retry")
}

func TestCompleteExternalRejectsInvalidLiveReadback(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*reconcileHarness)
	}{
		{
			name: "transport failure",
			configure: func(h *reconcileHarness) {
				h.client.beforeComplete = func() {
					h.client.readErr = &connector.Error{Code: "temporarily_unavailable", Message: "readback unavailable"}
				}
			},
		},
		{
			name: "identity changed",
			configure: func(h *reconcileHarness) {
				h.client.completeReadback = completedRoot(h.client.read, "wrong-identity")
				h.client.completeReadback.IdentityKey = "other-account"
			},
		},
		{
			name: "missing proof",
			configure: func(h *reconcileHarness) {
				h.client.completeReadback = completedRoot(h.client.read, "missing-proof")
				h.client.completeReadback.ObservedAt = time.Time{}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newReconcileHarness(t)
			_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
			require.NoError(t, err)
			test.configure(h)

			_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

			require.Error(t, err)
			assert.Equal(t, 1, h.client.completeCalls)
			failed, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
			require.NoError(t, readErr)
			assert.Equal(t, 1, failed.ConsecutiveFailures)
			assert.Empty(t, failed.ClaimToken)
		})
	}
}

func TestCompleteExternalFailureLeavesKataClosedAndRetryable(t *testing.T) {
	h := newReconcileHarness(t)
	_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	connectorErr := &connector.Error{Code: "temporarily_unavailable", Message: "completion unavailable"}
	h.client.completeErr = connectorErr

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	assert.ErrorIs(t, err, connectorErr)
	closed, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "closed", closed.Status)
	failed, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, 1, failed.ConsecutiveFailures)
	assert.Empty(t, failed.ClaimToken)
}

func TestCompleteExternalReopenRequestPrecedesRecompletion(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.read = completedRoot(h.client.read, "complete-before-close")
	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	_, _, _, err = h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.read.State = "open"
	h.client.read.Revision = "reopened-externally"
	h.client.read.UpdatedAt = h.boundAt.Add(3 * time.Minute)
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	h.client.completeReadback = completedRoot(h.client.read, "recompleted-by-kata")

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, result.ReopenRequests)
	assert.Equal(t, 1, h.client.completeCalls)
	comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	require.Len(t, comments, 2)
	assert.Contains(t, comments[1].Body, "review reopening")
	h.requireSuccessfulClaim(t, "complete", "recompleted-by-kata")
}

func TestCompleteExternalFieldConflictContinuesButFieldTransportFailureBlocks(t *testing.T) {
	t.Run("conflict continues", func(t *testing.T) {
		h := newReconcileHarness(t)
		h.mapField(t, "scheduled_on", "start-date")
		h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
		h.setKataField(t, "scheduled_on", date("2026-08-21"))
		h.client.fieldValues["start-date"] = date("2026-08-22")
		_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
		require.NoError(t, err)
		h.client.completeReadback = completedRoot(h.client.read, "complete-after-conflict")

		result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

		require.NoError(t, err)
		assert.Equal(t, 1, result.FieldConflicts)
		assert.Equal(t, 1, h.client.completeCalls)
	})

	t.Run("transport failure blocks", func(t *testing.T) {
		h := newReconcileHarness(t)
		h.mapField(t, "scheduled_on", "start-date")
		h.client.readFieldsErr = map[string]error{
			"start-date": &connector.Error{Code: "temporarily_unavailable", Message: "field unavailable"},
		}
		_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
		require.NoError(t, err)
		h.client.completeReadback = completedRoot(h.client.read, "must-not-complete")

		_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

		require.Error(t, err)
		assert.Zero(t, h.client.completeCalls)
	})
}

func TestCompleteExternalLateFailurePreservesCommittedStagesAndFinalizesClaim(t *testing.T) {
	for _, test := range []struct {
		name   string
		failed error
		cancel bool
	}{
		{name: "connector failure", failed: errors.New("completion failed")},
		{name: "cancellation", failed: context.Canceled, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPublishingReconcileHarness(t)
			h.client.read.Title = "Projected before completion"
			h.client.read.Revision = "root-before-completion"
			h.client.read.UpdatedAt = h.boundAt.Add(2 * time.Minute)
			h.client.read.ObservedAt = h.client.read.UpdatedAt
			h.client.comments = []connector.Comment{{
				ID: "comment-before-completion", Body: "Inbound before completion",
				Author:    connector.Actor{ID: "actor-7", DisplayName: "Reviewer"},
				CreatedAt: h.boundAt.Add(time.Minute), UpdatedAt: h.boundAt.Add(time.Minute),
			}}
			h.mapField(t, "scheduled_on", "start-date")
			h.client.fieldValues["start-date"] = date("2026-08-21")
			local, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: h.issue.ID, Author: "tester", Body: "Outbound before completion",
			})
			require.NoError(t, err)
			h.client.publishResult = publishedComment(h, "published-before-completion", local.Body)
			_, _, _, err = h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
			require.NoError(t, err)
			ctx := t.Context()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				h.client.beforeComplete = cancel
			}
			h.client.completeErr = test.failed

			result, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{})

			require.ErrorIs(t, err, test.failed)
			assert.Contains(t, eventTypes(result.Events), "issue.updated")
			assert.Contains(t, eventTypes(result.Events), "issue.commented")
			assert.Contains(t, eventTypes(result.Events), "issue.metadata_updated")
			assert.Equal(t, 1, result.CommentsCreated)
			assert.Equal(t, 1, h.client.publishCalls)
			_, readErr := h.store.ImportMappingBySource(
				t.Context(), h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), "comment", "published-before-completion",
			)
			require.NoError(t, readErr)
			stored, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.Equal(t, "closed", stored.Status)
			assert.Equal(t, "Projected before completion", stored.Title)
			field, readErr := fieldCodecs["scheduled_on"].ReadKata(stored)
			require.NoError(t, readErr)
			assert.Equal(t, date("2026-08-21"), field)
			binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
			require.NoError(t, readErr)
			assert.Empty(t, binding.ClaimToken)
			assert.Equal(t, 1, binding.ConsecutiveFailures)
			assert.NotEmpty(t, binding.LastError)
		})
	}
}

func completedRoot(root connector.Root, revision string) connector.Root {
	root.State = "complete"
	root.Revision = revision
	root.UpdatedAt = root.UpdatedAt.Add(time.Minute)
	root.ObservedAt = root.ObservedAt.Add(time.Minute)
	return root
}

type afterPublishedCommentBarrierStorage struct {
	db.Storage
	entered chan struct{}
	release chan struct{}
}

type mutateCompletionBindingReadStorage struct {
	db.Storage
	reads  int
	mutate func(*db.ExternalRootBinding)
}

func (s *mutateCompletionBindingReadStorage) ExternalRootBindingByID(
	ctx context.Context,
	bindingID int64,
) (db.ExternalRootBinding, error) {
	binding, err := s.Storage.ExternalRootBindingByID(ctx, bindingID)
	s.reads++
	if err == nil && s.reads > 1 {
		if s.mutate != nil {
			s.mutate(&binding)
		} else {
			binding.ExternalRootKey = "other-root"
		}
	}
	return binding, err
}

func (s *afterPublishedCommentBarrierStorage) ClearPendingExternalComment(
	ctx context.Context,
	params db.ClearPendingExternalCommentParams,
) (db.ExternalRootBinding, db.Event, error) {
	binding, event, err := s.Storage.ClearPendingExternalComment(ctx, params)
	if err == nil && params.Action == "published" {
		close(s.entered)
		<-s.release
	}
	return binding, event, err
}

func TestReconcileCompletionAndReopeningRequestsAreDeduplicatedAndAdditive(t *testing.T) {
	h := newReconcileHarness(t)
	metadataBefore := h.setAttention(t)
	h.client.read.State = "complete"
	h.client.read.Revision = "complete-revision-1"
	h.client.read.Actor = &connector.Actor{ID: "actor-8", DisplayName: "Verifier"}

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	second, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, first.CompletionRequests)
	assert.Equal(t, 0, second.CompletionRequests)

	issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
	assert.JSONEq(t, string(metadataBefore), string(issue.Metadata))
	hasReview, err := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, err)
	assert.True(t, hasReview)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Contains(t, comments[0].Body, "Verifier")
	assert.Contains(t, comments[0].Body, "verify completion")
	_, err = h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootLifecycleMappingSource(h.binding), "comment",
		"lifecycle:"+h.binding.UID+":complete:complete-revision-1",
	)
	require.NoError(t, err)

	_, _, _, err = h.store.CloseIssue(t.Context(), h.issue.ID, "done", "tester", "Verified externally requested completion.", nil)
	require.NoError(t, err)
	h.client.read.State = "open"
	h.client.read.Revision = "open-revision-2"
	h.client.read.Actor = &connector.Actor{ID: "actor-9", DisplayName: "Coordinator"}
	h.client.completeReadback = completedRoot(h.client.read, "recompleted-after-review")
	reopened, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	duplicate, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, reopened.ReopenRequests)
	assert.Equal(t, 0, duplicate.ReopenRequests)
	assert.Equal(t, 1, h.client.completeCalls)

	issue, err = h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "closed", issue.Status)
	assert.JSONEq(t, string(metadataBefore), string(issue.Metadata))
	hasReview, err = h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, err)
	assert.True(t, hasReview)
	comments, err = h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	assert.Contains(t, comments[1].Body, "Coordinator")
	assert.Contains(t, comments[1].Body, "review reopening")
}

func TestReconcileLifecycleRequestIdentityIsScopedToBinding(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.read.State = "complete"
	h.client.read.Revision = "shared-provider-revision"
	h.client.read.UpdatedAt = h.boundAt.Add(time.Minute)
	h.client.read.ObservedAt = h.boundAt.Add(time.Minute)

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, first.CompletionRequests)

	secondIssue, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.project.ID, Title: "Second local plan", Author: "tester",
	})
	require.NoError(t, err)
	secondBinding, _, err := h.store.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: h.project.ID, IssueID: secondIssue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-2", ExternalAccountKey: "account-1",
		Actor: "tester", ReceiveCommentsAfter: h.boundAt,
	})
	require.NoError(t, err)
	h.client.read.Key = "root-2"

	second, err := h.reconciler.Run(t.Context(), secondBinding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, second.CompletionRequests)
	comments, err := h.store.CommentsByIssue(t.Context(), secondIssue.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 1)
	_, err = h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootLifecycleMappingSource(secondBinding), "comment",
		"lifecycle:"+secondBinding.UID+":complete:shared-provider-revision",
	)
	require.NoError(t, err)
}

func TestReconcileLifecycleTransitionSurvivesLaterFailureAndRevisionChange(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.client.read.State = "complete"
	h.client.read.Revision = "complete-revision-1"
	h.client.read.UpdatedAt = h.boundAt.Add(time.Minute)
	h.client.read.ObservedAt = h.boundAt.Add(time.Minute)
	h.client.listFieldsErr = &connector.Error{Code: "temporarily_unavailable", Message: "fields unavailable"}

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.Error(t, err)
	assert.Equal(t, 1, first.CompletionRequests)
	failed := h.requireBinding(t)
	assert.Equal(t, "complete", failed.LastExternalState)
	assert.Equal(t, "complete-revision-1", failed.LastExternalRevision)

	h.client.listFieldsErr = nil
	h.client.read.Revision = "complete-revision-2"
	h.client.read.UpdatedAt = h.client.read.UpdatedAt.Add(time.Minute)
	h.client.read.ObservedAt = h.client.read.ObservedAt.Add(time.Minute)

	second, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, second.CompletionRequests)
	comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Len(t, comments, 1)
}

func TestFieldReconcileInitialAndSteadyMatrix(t *testing.T) {
	cases := []struct {
		name          string
		baseline      *connector.FieldValue
		kata          connector.FieldValue
		external      connector.FieldValue
		want          connector.FieldValue
		wantConflict  bool
		wantKataWrite bool
		wantExtWrite  bool
	}{
		{name: "initial same", kata: date("2026-08-20"), external: date("2026-08-20"), want: date("2026-08-20")},
		{name: "initial Kata only", kata: date("2026-08-20"), external: nullValue(), want: date("2026-08-20"), wantExtWrite: true},
		{name: "initial external only", kata: nullValue(), external: date("2026-08-20"), want: date("2026-08-20"), wantKataWrite: true},
		{name: "initial divergent", kata: date("2026-08-20"), external: date("2026-08-21"), wantConflict: true},
		{name: "steady unchanged", baseline: new(date("2026-08-20")), kata: date("2026-08-20"), external: date("2026-08-20"), want: date("2026-08-20")},
		{name: "steady Kata only", baseline: new(date("2026-08-20")), kata: date("2026-08-21"), external: date("2026-08-20"), want: date("2026-08-21"), wantExtWrite: true},
		{name: "steady external only", baseline: new(date("2026-08-20")), kata: date("2026-08-20"), external: date("2026-08-21"), want: date("2026-08-21"), wantKataWrite: true},
		{name: "steady same change", baseline: new(date("2026-08-20")), kata: date("2026-08-21"), external: date("2026-08-21"), want: date("2026-08-21")},
		{name: "steady divergent", baseline: new(date("2026-08-20")), kata: date("2026-08-21"), external: date("2026-08-22"), wantConflict: true},
		{name: "Kata clear", baseline: new(date("2026-08-20")), kata: nullValue(), external: date("2026-08-20"), want: nullValue(), wantExtWrite: true},
		{name: "external clear", baseline: new(date("2026-08-20")), kata: date("2026-08-20"), external: nullValue(), want: nullValue(), wantKataWrite: true},
		{name: "clear versus change", baseline: new(date("2026-08-20")), kata: nullValue(), external: date("2026-08-22"), wantConflict: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newReconcileHarness(t)
			h.mapField(t, "scheduled_on", "start-date")
			h.setKataField(t, "scheduled_on", test.kata)
			h.client.fieldValues["start-date"] = test.external
			if test.baseline != nil {
				h.seedBaseline(t, "scheduled_on", *test.baseline)
			}

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
			require.NoError(t, err)
			if test.wantConflict {
				assert.Equal(t, 1, result.FieldConflicts)
				state := h.fieldState(t, "scheduled_on")
				assert.True(t, state.Conflicted)
				assert.Equal(t, test.kata, decodeStoredFieldValue(t, state.ConflictKata))
				assert.Equal(t, test.external, decodeStoredFieldValue(t, state.ConflictExternal))
				return
			}

			assert.Zero(t, result.FieldConflicts)
			state := h.fieldState(t, "scheduled_on")
			assert.False(t, state.Conflicted)
			assert.Equal(t, test.want, decodeStoredFieldValue(t, state.Baseline))
			issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			kataValue, readErr := fieldCodecs["scheduled_on"].ReadKata(issue)
			require.NoError(t, readErr)
			assert.Equal(t, test.want, kataValue)
			assert.Equal(t, test.want, h.client.fieldValues["start-date"])
			assert.Equal(t, test.wantExtWrite, len(h.client.writeFieldCalls) == 1)
			if test.wantExtWrite {
				assert.Equal(t, map[string]connector.FieldValue{"start-date": test.external}, h.client.writeFieldCalls[0].Expected)
			}
			assert.Equal(t, test.wantKataWrite, containsEventType(result.Events, "issue.metadata_updated"))
		})
	}
}

func TestFieldReconcileRejectsConcurrentLocalMetadataEdit(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.setKataField(t, "scheduled_on", date("2026-08-20"))
	h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
	h.client.fieldValues["start-date"] = date("2026-08-21")
	storage := &editBeforeFieldProjectionStorage{Storage: h.store, issueID: h.issue.ID}
	h.reconciler.store = storage

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	var revisionConflict *db.RevisionConflictError
	require.ErrorAs(t, err, &revisionConflict)
	assert.NotContains(t, eventTypes(result.Events), "issue.metadata_updated")
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	value, readErr := fieldCodecs["scheduled_on"].ReadKata(issue)
	require.NoError(t, readErr)
	assert.Equal(t, date("2026-08-22"), value)
	state := h.fieldState(t, "scheduled_on")
	assert.Equal(t, date("2026-08-20"), decodeStoredFieldValue(t, state.Baseline))
	assert.False(t, state.Conflicted)
}

func TestFieldReconcileFailsWhenMappedCapabilityIsRemoved(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.client.description.Capabilities = nil
	listCalls := h.client.listFieldsCalls
	h.client.read.Title = "Inbound projection still runs"
	h.client.read.State = "complete"
	h.client.read.Revision = "complete-without-fields-capability"
	h.client.read.UpdatedAt = h.now
	h.client.read.ObservedAt = h.now

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.ErrorIs(t, err, ErrFieldSynchronizationUnavailable)
	assert.True(t, result.RootUpdated)
	assert.Equal(t, 1, result.CompletionRequests)
	assert.Equal(t, listCalls, h.client.listFieldsCalls)
	assert.Empty(t, h.client.readFieldCalls)
	assert.Empty(t, h.client.writeFieldCalls)
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "Inbound projection still runs", issue.Title)
	binding := h.requireBinding(t)
	assert.Nil(t, binding.LastSuccessAt)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
}

func TestFieldConflictPausesOnlyThatField(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.mapField(t, "deadline_on", "due-date")
	h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
	h.setKataField(t, "scheduled_on", date("2026-08-21"))
	h.client.fieldValues["start-date"] = date("2026-08-22")
	h.client.fieldValues["due-date"] = date("2026-08-23")
	h.client.comments = []connector.Comment{{
		ID: "comment-8", Body: "External comment",
		Author:    connector.Actor{ID: "actor-8", DisplayName: "Reviewer"},
		CreatedAt: h.boundAt.Add(time.Minute), UpdatedAt: h.boundAt.Add(time.Minute),
	}}
	h.client.read.State = "complete"
	h.client.read.Revision = "complete-with-field-conflict"
	h.client.read.UpdatedAt = h.boundAt.Add(2 * time.Minute)
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	local, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: h.issue.ID, Author: "tester", Body: "Local outbound comment",
	})
	require.NoError(t, err)
	h.client.publishResult = publishedComment(h, "published-field-conflict", local.Body)

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FieldConflicts)
	assert.Equal(t, 1, result.CommentsCreated)
	assert.Equal(t, 1, result.CompletionRequests)
	assert.Equal(t, 1, h.client.publishCalls)
	_, err = h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), "comment", "published-field-conflict",
	)
	require.NoError(t, err)
	assert.True(t, h.fieldState(t, "scheduled_on").Conflicted)
	assert.False(t, h.fieldState(t, "deadline_on").Conflicted)
	issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	deadline, err := fieldCodecs["deadline_on"].ReadKata(issue)
	require.NoError(t, err)
	assert.Equal(t, date("2026-08-23"), deadline)
	binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	assert.True(t, binding.Enabled)
	assert.Empty(t, binding.PausedReason)
}

func TestFieldConflictIsIdempotentAndRefreshesCandidatesWithoutAnotherEvent(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
	h.setKataField(t, "scheduled_on", date("2026-08-21"))
	h.client.fieldValues["start-date"] = date("2026-08-22")

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, first.FieldConflicts)
	firstState := h.fieldState(t, "scheduled_on")
	h.setKataField(t, "scheduled_on", date("2026-08-23"))
	h.client.fieldValues["start-date"] = date("2026-08-24")
	second, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, second.FieldConflicts)
	assert.NotContains(t, eventTypes(second.Events), "issue.external_field_conflicted")
	secondState := h.fieldState(t, "scheduled_on")
	assert.NotEqual(t, string(firstState.ConflictKata), string(secondState.ConflictKata))
	assert.NotEqual(t, string(firstState.ConflictExternal), string(secondState.ConflictExternal))
	assert.Equal(t, date("2026-08-23"), decodeStoredFieldValue(t, secondState.ConflictKata))
	assert.Equal(t, date("2026-08-24"), decodeStoredFieldValue(t, secondState.ConflictExternal))
}

func TestFieldReconcileDescriptorAndValueDriftBecomeSafeConflicts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*reconcileHarness)
	}{
		{name: "schema revision", mutate: func(h *reconcileHarness) { h.client.fields[0].SchemaRevision = "schema-2" }},
		{name: "accepted kinds", mutate: func(h *reconcileHarness) { h.client.fields[0].AcceptedKinds = []string{fieldKindDate} }},
		{name: "nullability", mutate: func(h *reconcileHarness) { h.client.fields[0].Nullable = false }},
		{name: "writability", mutate: func(h *reconcileHarness) { h.client.fields[0].Writable = false }},
		{name: "missing stable id", mutate: func(h *reconcileHarness) { h.client.fields[0].ID = "different-id" }},
		{name: "invalid external value", mutate: func(h *reconcileHarness) {
			h.client.fieldValues["start-date"] = connector.FieldValue{Kind: "text", Value: "untrusted-raw-value"}
		}},
		{name: "missing external value kind", mutate: func(h *reconcileHarness) {
			h.client.fieldValues["start-date"] = connector.FieldValue{}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newReconcileHarness(t)
			h.mapField(t, "scheduled_on", "start-date")
			h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
			h.setKataField(t, "scheduled_on", date("2026-08-21"))
			h.client.fieldValues["start-date"] = date("2026-08-20")
			test.mutate(h)

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, result.FieldConflicts)
			state := h.fieldState(t, "scheduled_on")
			assert.True(t, state.Conflicted)
			assert.Equal(t, date("2026-08-21"), decodeStoredFieldValue(t, state.ConflictKata))
			assert.Equal(t, date("2026-08-20"), decodeStoredFieldValue(t, state.ConflictExternal))
			assert.NotContains(t, string(state.ConflictExternal), "untrusted-raw-value")
			assert.Empty(t, h.client.writeFieldCalls)
			if test.name != "invalid external value" && test.name != "missing external value kind" {
				assert.Empty(t, h.client.readFieldCalls)
			}
		})
	}
}

func TestFieldReconcileRejectsNonCanonicalLiveDescriptorIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*reconcileHarness)
	}{
		{name: "padded field ID", mutate: func(h *reconcileHarness) { h.client.fields[0].ID = " start-date " }},
		{name: "padded schema revision", mutate: func(h *reconcileHarness) { h.client.fields[0].SchemaRevision = " schema-1 " }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newReconcileHarness(t)
			h.mapField(t, "scheduled_on", "start-date")
			test.mutate(h)

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
			require.ErrorIs(t, err, connectorclient.ErrProtocolFailure)
			assert.Zero(t, result.FieldConflicts)
			assert.Empty(t, h.client.readFieldCalls)
			assert.Empty(t, h.client.writeFieldCalls)
			binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
			require.NoError(t, readErr)
			assert.Equal(t, "external connector protocol failed", binding.LastError)
			assert.Empty(t, binding.ClaimToken)
		})
	}
}

func TestFieldReconcileMissingCodecRetainsEstablishedBaseline(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	seeded := h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
	codec := fieldCodecs["scheduled_on"]
	delete(fieldCodecs, "scheduled_on")
	t.Cleanup(func() { fieldCodecs["scheduled_on"] = codec })

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, first.FieldConflicts)
	assert.Contains(t, eventTypes(first.Events), "issue.external_field_conflicted")
	stored := h.fieldState(t, "scheduled_on")
	assert.True(t, stored.Conflicted)
	assert.Equal(t, string(seeded.Baseline), string(stored.Baseline))
	assert.Equal(t, nullFieldValue(), decodeStoredFieldValue(t, stored.ConflictKata))
	assert.Equal(t, nullFieldValue(), decodeStoredFieldValue(t, stored.ConflictExternal))

	second, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, second.FieldConflicts)
	assert.NotContains(t, eventTypes(second.Events), "issue.external_field_conflicted")
	repeated := h.fieldState(t, "scheduled_on")
	assert.Equal(t, string(seeded.Baseline), string(repeated.Baseline))
	binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	assert.True(t, binding.Active)
	assert.True(t, binding.Enabled)
}

func TestFieldReconcileRefreshesEachMappedDescriptorIndependently(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.mapField(t, "deadline_on", "due-date")
	h.client.fields[0].SchemaRevision = "schema-2"
	h.client.fieldValues["start-date"] = date("2026-08-21")
	h.client.fieldValues["due-date"] = date("2026-08-22")

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FieldConflicts)
	assert.True(t, h.fieldState(t, "scheduled_on").Conflicted)
	assert.False(t, h.fieldState(t, "deadline_on").Conflicted)
	assert.Len(t, h.client.readFieldCalls, 1)
	assert.Equal(t, []string{"due-date"}, h.client.readFieldCalls[0].FieldIDs)
}

func TestFieldReconcileLossyWriteReadbackDoesNotAdvanceBaseline(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.setKataField(t, "scheduled_on", date("2026-08-21"))
	h.client.fieldValues["start-date"] = nullValue()
	h.client.writeFieldResult = func(_ string, _ connector.FieldValue) connector.FieldValue {
		return date("2026-08-22")
	}

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.FieldConflicts)
	state := h.fieldState(t, "scheduled_on")
	assert.True(t, state.Conflicted)
	assert.Empty(t, state.Baseline)
	assert.Equal(t, date("2026-08-21"), decodeStoredFieldValue(t, state.ConflictKata))
	assert.Equal(t, date("2026-08-22"), decodeStoredFieldValue(t, state.ConflictExternal))
}

func TestFieldReconcilePreservesConnectorErrorsWithoutAdvancingBaseline(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		h := newReconcileHarness(t)
		h.mapField(t, "scheduled_on", "start-date")
		connectorErr := &connector.Error{Code: "temporarily_unavailable", Message: "field read unavailable"}
		h.client.readFieldsErr = map[string]error{"start-date": connectorErr}

		_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
		assert.ErrorIs(t, err, connectorErr)
		states, readErr := h.store.ExternalFieldStates(t.Context(), h.binding.ID)
		require.NoError(t, readErr)
		assert.Empty(t, states)
	})

	t.Run("write", func(t *testing.T) {
		h := newReconcileHarness(t)
		h.mapField(t, "scheduled_on", "start-date")
		h.setKataField(t, "scheduled_on", date("2026-08-21"))
		h.client.fieldValues["start-date"] = nullValue()
		connectorErr := &connector.Error{Code: "temporarily_unavailable", Message: "field write unavailable"}
		h.client.writeFieldsErr = connectorErr

		_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
		assert.ErrorIs(t, err, connectorErr)
		states, readErr := h.store.ExternalFieldStates(t.Context(), h.binding.ID)
		require.NoError(t, readErr)
		assert.Empty(t, states)
	})
}

func TestFieldReconcilePreservesTimezoneAndRejectsIncompatibleLocalSiblings(t *testing.T) {
	t.Run("date instant and clear preserve issue timezone", func(t *testing.T) {
		for _, value := range []connector.FieldValue{
			date("2026-08-21"),
			{Kind: fieldKindInstant, Value: "2026-08-21T10:30:00Z"},
			nullValue(),
		} {
			h := newReconcileHarness(t)
			h.mapField(t, "scheduled_on", "start-date")
			h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
			_, err := h.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
				IssueID: h.issue.ID, Actor: "tester", Patch: map[string]json.RawMessage{
					"scheduled_on": json.RawMessage(`"2026-08-20"`),
					"timezone":     json.RawMessage(`"America/New_York"`),
				},
			})
			require.NoError(t, err)
			h.client.fieldValues["start-date"] = value

			_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
			require.NoError(t, err)
			issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, err)
			values, err := metadataObject(issue.Metadata)
			require.NoError(t, err)
			assert.JSONEq(t, `"America/New_York"`, string(values["timezone"]))
		}
	})

	t.Run("UTC and null sibling do not pin a new local timezone", func(t *testing.T) {
		for _, sibling := range []connector.FieldValue{
			{Kind: fieldKindInstant, Value: "2026-08-21T10:30:00Z"},
			nullValue(),
		} {
			h := newReconcileHarness(t)
			h.mapField(t, "scheduled_on", "start-date")
			h.setKataField(t, "deadline_on", sibling)
			_, err := h.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
				IssueID: h.issue.ID, Actor: "tester", Patch: map[string]json.RawMessage{
					"timezone": json.RawMessage(`"America/New_York"`),
				},
			})
			require.NoError(t, err)
			h.client.fieldValues["start-date"] = connector.FieldValue{
				Kind: fieldKindLocalDateTime, Value: "2026-08-21T09:30", Timezone: "America/Los_Angeles",
			}

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
			require.NoError(t, err)
			assert.Zero(t, result.FieldConflicts)
			issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, err)
			values, err := metadataObject(issue.Metadata)
			require.NoError(t, err)
			assert.JSONEq(t, `"America/Los_Angeles"`, string(values["timezone"]))
		}
	})

	t.Run("two local fields with different zones remain independent", func(t *testing.T) {
		h := newReconcileHarness(t)
		h.mapField(t, "scheduled_on", "start-date")
		h.mapField(t, "deadline_on", "due-date")
		h.client.fieldValues["start-date"] = connector.FieldValue{
			Kind: fieldKindLocalDateTime, Value: "2026-08-21T09:30", Timezone: "America/New_York",
		}
		h.client.fieldValues["due-date"] = connector.FieldValue{
			Kind: fieldKindLocalDateTime, Value: "2026-08-21T10:30", Timezone: "America/Los_Angeles",
		}

		result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, result.FieldConflicts)
		assert.False(t, h.fieldState(t, "scheduled_on").Conflicted)
		assert.True(t, h.fieldState(t, "deadline_on").Conflicted)
	})

	t.Run("two mapped local fields move to one new timezone together", func(t *testing.T) {
		h := newReconcileHarness(t)
		h.mapField(t, "scheduled_on", "start-date")
		h.mapField(t, "deadline_on", "due-date")
		oldStart := connector.FieldValue{
			Kind: fieldKindLocalDateTime, Value: "2026-08-21T09:30", Timezone: "America/New_York",
		}
		oldDue := connector.FieldValue{
			Kind: fieldKindLocalDateTime, Value: "2026-08-21T10:30", Timezone: "America/New_York",
		}
		h.setKataField(t, "scheduled_on", oldStart)
		h.setKataField(t, "deadline_on", oldDue)
		h.seedBaseline(t, "scheduled_on", oldStart)
		h.seedBaseline(t, "deadline_on", oldDue)
		newStart := connector.FieldValue{
			Kind: fieldKindLocalDateTime, Value: "2026-08-22T09:30", Timezone: "America/Los_Angeles",
		}
		newDue := connector.FieldValue{
			Kind: fieldKindLocalDateTime, Value: "2026-08-22T10:30", Timezone: "America/Los_Angeles",
		}
		h.client.fieldValues["start-date"] = newStart
		h.client.fieldValues["due-date"] = newDue

		result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

		require.NoError(t, err)
		assert.Zero(t, result.FieldConflicts)
		issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
		require.NoError(t, readErr)
		values, readErr := metadataObject(issue.Metadata)
		require.NoError(t, readErr)
		assert.JSONEq(t, `"2026-08-22T09:30"`, string(values["scheduled_on"]))
		assert.JSONEq(t, `"2026-08-22T10:30"`, string(values["deadline_on"]))
		assert.JSONEq(t, `"America/Los_Angeles"`, string(values["timezone"]))
		assert.Equal(t, newStart, decodeStoredFieldValue(t, h.fieldState(t, "scheduled_on").Baseline))
		assert.Equal(t, newDue, decodeStoredFieldValue(t, h.fieldState(t, "deadline_on").Baseline))
	})
}

func TestReconcileFirstBindOfCompleteRootUsesReservedClaim(t *testing.T) {
	h := newServiceHarness(t)
	h.client.resolved.State = "complete"
	h.client.resolved.Revision = "complete-on-bind"
	h.client.resolved.UpdatedAt = testObservedAt
	h.client.resolved.Actor = &connector.Actor{ID: "actor-3", DisplayName: "Verifier"}
	h.client.read = h.client.resolved
	reconciler := NewReconciler(h.observed, h.registry, ReconcilerConfig{Now: func() time.Time {
		return testObservedAt.Add(time.Minute)
	}})
	h.service.immediateClaimedReconcile = func(ctx context.Context, bindingID int64, claimToken string) ([]db.Event, error) {
		result, err := reconciler.Run(ctx, bindingID, RunOptions{ClaimToken: claimToken})
		return result.Events, err
	}

	binding, events, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	assert.Empty(t, binding.ClaimToken)
	assert.NotNil(t, binding.LastSuccessAt)
	assert.Contains(t, eventTypes(events), "issue.updated")
	assert.Contains(t, eventTypes(events), "issue.commented")
	assert.Contains(t, eventTypes(events), "issue.labeled")
	issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
}

func TestReconcileClaimedRejectsWrongTokenWithoutMutation(t *testing.T) {
	h := newReconcileHarnessWithInitialClaim(t, "reserved-claim")
	h.client.read.Title = "Must not project"

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{ClaimToken: "wrong-claim"})
	assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, h.issue.Title, issue.Title)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "reserved-claim", binding.ClaimToken)
}

func TestReconcileClaimedPreCanceledContextFinalizesReservedClaim(t *testing.T) {
	h := newReconcileHarnessWithInitialClaim(t, "reserved-claim")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{ClaimToken: "reserved-claim"})

	require.ErrorIs(t, err, context.Canceled)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	assert.Contains(t, binding.LastError, context.Canceled.Error())
}

func TestReconcileDeletedMissingAndIdentityChangedRootsPauseWithoutIssueMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
		mutate func(*reconcileHarness)
	}{
		{name: "deleted", reason: "external_root_deleted", mutate: func(h *reconcileHarness) {
			h.client.read.State = "deleted"
		}},
		{name: "missing", reason: "external_root_missing", mutate: func(h *reconcileHarness) {
			h.client.readErr = &connector.Error{Code: "not_found", Message: "root unavailable"}
		}},
		{name: "identity changed", reason: "external_root_identity_changed", mutate: func(h *reconcileHarness) {
			h.client.read.IdentityKey = "different-account"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newReconcileHarness(t)
			metadataBefore := h.setAttention(t)
			h.client.read.Title = "Must not project"
			test.mutate(h)

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
			require.NoError(t, err)
			assert.True(t, result.Paused)
			issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.Equal(t, h.issue.Title, issue.Title)
			assert.Equal(t, h.issue.Body, issue.Body)
			assert.JSONEq(t, string(metadataBefore), string(issue.Metadata))
			comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.Empty(t, comments)
			hasReview, readErr := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
			require.NoError(t, readErr)
			assert.False(t, hasReview)
			binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
			require.NoError(t, readErr)
			assert.False(t, binding.Enabled)
			assert.Empty(t, binding.ClaimToken)
			assert.Equal(t, test.reason, binding.PausedReason)
		})
	}
}

func TestReconcileFailureRecordsSafeErrorAndReleasesClaim(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.readErr = errors.New("connector unavailable")

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.Error(t, err)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.True(t, binding.Enabled)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	assert.Equal(t, "external connector request failed", binding.LastError)
}

func TestReconcileFailureFinalizationIgnoresCallerCancellation(t *testing.T) {
	h := newReconcileHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.client.beforeReadReturn = cancel
	h.client.readErr = context.Canceled

	_, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{})

	require.ErrorIs(t, err, context.Canceled)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	assert.Equal(t, context.Canceled.Error(), binding.LastError)
}

func TestReconcileParentCancellationDuringDescribeDoesNotMutateRegistryHealth(t *testing.T) {
	h := newReconcileHarness(t)
	h.registry.recordDescribeHealthError("notes", ErrConnectorCall)
	wantHealth, ok := h.registry.Snapshot("notes")
	require.True(t, ok)
	require.NotEmpty(t, wantHealth.HealthError)
	ctx, cancel := context.WithCancel(t.Context())
	h.client.beforeDescribeReturn = cancel
	h.client.describeErr = errors.New("opaque connector cancellation detail")

	_, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{})

	require.ErrorIs(t, err, context.Canceled)
	snapshot, ok := h.registry.Snapshot("notes")
	require.True(t, ok)
	assert.Equal(t, wantHealth.HealthError, snapshot.HealthError)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	assert.Equal(t, context.Canceled.Error(), binding.LastError)
}

func TestReconcileConnectorFailureRemainsHealthWhenParentExpiresAfterBoundary(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.describeErr = errors.Join(connectorclient.ErrRequestTimeout, context.DeadlineExceeded)
	ctx := newControlledEndContext(t.Context())
	h.registry.mu.Lock()
	instance := h.registry.instances["notes"]
	instance.Client = &boundaryReturnHookClient{
		Client: instance.Client,
		afterDescribe: func() {
			ctx.end(context.DeadlineExceeded)
		},
	}
	h.registry.instances["notes"] = instance
	h.registry.mu.Unlock()

	_, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{})

	require.ErrorIs(t, err, ErrConnectorCall)
	require.ErrorIs(t, err, connectorclient.ErrRequestTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	snapshot, ok := h.registry.Snapshot("notes")
	require.True(t, ok)
	assert.Equal(t, "external connector request timed out", snapshot.HealthError)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	assert.Equal(t, "external connector request timed out", binding.LastError)
}

func TestReconcileFailureFinalizationIgnoresMalformedVerifiedRoot(t *testing.T) {
	h := newReconcileHarness(t)
	const claimToken = "malformed-verified-root" // #nosec G101 -- synthetic claim fixture, not a credential.
	_, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, claimToken, h.now, h.now.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	claimHeld := true
	failure := errors.New("outbound publication failed")

	err = h.reconciler.recordFailure(
		t.Context(), h.binding, claimToken, failure,
		&connector.Root{State: "complete"}, &claimHeld,
	)

	assert.ErrorIs(t, err, failure)
	assert.NotErrorIs(t, err, db.ErrExternalRootValidation)
	assert.False(t, claimHeld)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	assert.Equal(t, failure.Error(), binding.LastError)
	assert.Equal(t, &h.now, binding.LastAttemptAt)
	assert.Equal(t, &h.now, binding.NextAttemptAt)
	assert.Nil(t, binding.LastSuccessAt)
}

func TestReconcileTypedConnectorErrorAfterCallerCancellationUsesCancellationPath(t *testing.T) {
	h := newReconcileHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.client.beforeReadReturn = cancel
	h.client.readErr = &connector.Error{Code: "not_found", Message: "root unavailable"}

	result, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{})

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, result.Paused)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.True(t, binding.Enabled)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	assert.Equal(t, context.Canceled.Error(), binding.LastError)
}

func TestReconcileSuccessFinalizationIgnoresCancellationAfterCommittedProjection(t *testing.T) {
	h := newReconcileHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelAfterRootProjectionStorage{Storage: h.store, cancel: cancel}
	h.reconciler.store = store
	h.client.read.Title = "Projected before cancellation"
	h.client.read.Revision = "root-revision-2"
	h.client.read.UpdatedAt = h.boundAt.Add(time.Minute)
	h.client.read.ObservedAt = h.client.read.UpdatedAt

	result, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.True(t, result.RootUpdated)
	assert.NoError(t, store.successCtxErr)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.NotNil(t, binding.LastSuccessAt)
	assert.Equal(t, "root-revision-2", binding.LastExternalRevision)
}

func TestFieldReconcileCancellationAfterNativeProjectionPreservesEventAndFinalizesClaim(t *testing.T) {
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.client.fieldValues["start-date"] = date("2026-08-21")
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelAfterFieldProjectionStorage{Storage: h.store, cancel: cancel}
	h.reconciler.store = store

	result, err := h.reconciler.Run(ctx, h.binding.ID, RunOptions{})

	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, eventTypes(result.Events), "issue.metadata_updated")
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, 1, binding.ConsecutiveFailures)
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	value, readErr := fieldCodecs["scheduled_on"].ReadKata(issue)
	require.NoError(t, readErr)
	assert.Equal(t, date("2026-08-21"), value)
}

func TestReconcileSteadyCompleteDoesNotRestoreVerifierRemovedReviewLabel(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.read.State = "complete"
	h.client.read.Revision = "complete-revision-1"

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, first.CompletionRequests)
	_, err = h.store.RemoveLabelAndEvent(t.Context(), h.issue.ID, db.LabelEventParams{
		EventType: "issue.unlabeled", Label: "needs-review", Actor: "verifier",
	})
	require.NoError(t, err)

	steady, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, steady.CompletionRequests)
	assert.NotContains(t, eventTypes(steady.Events), "issue.labeled")
	hasReview, err := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, err)
	assert.False(t, hasReview)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 1)
}

func TestReconcileLifecycleRetryDoesNotRestoreVerifierRemovedReviewLabel(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.read.State = "complete"
	h.client.read.Revision = "complete-revision-1"
	terminalErr := errors.New("record terminal success")
	h.reconciler.store = &failExternalRootSuccessOnceStorage{Storage: h.store, err: terminalErr}

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.ErrorIs(t, err, terminalErr)
	require.Equal(t, 1, first.CompletionRequests)
	_, err = h.store.RemoveLabelAndEvent(t.Context(), h.issue.ID, db.LabelEventParams{
		EventType: "issue.unlabeled", Label: "needs-review", Actor: "verifier",
	})
	require.NoError(t, err)

	retry, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, retry.CompletionRequests)
	assert.NotContains(t, eventTypes(retry.Events), "issue.labeled")
	hasReview, err := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, err)
	assert.False(t, hasReview)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 1)
}

func TestReconcileLocalCompletionRetryPreservesVerifiedRootAfterSuccessWriteFailure(t *testing.T) {
	h := newReconcileHarness(t)
	_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.completeReadback = completedRoot(h.client.read, "completed-by-kata")
	terminalErr := errors.New("record terminal success")
	h.reconciler.store = &failExternalRootSuccessOnceStorage{Storage: h.store, err: terminalErr}

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.ErrorIs(t, err, terminalErr)
	assert.Zero(t, first.CompletionRequests)
	assert.Equal(t, 1, h.client.completeCalls)
	failed, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "complete", failed.LastExternalState)
	assert.Equal(t, "completed-by-kata", failed.LastExternalRevision)
	assert.Equal(t, 1, failed.ConsecutiveFailures)
	assert.Nil(t, failed.LastSuccessAt)

	retry, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, retry.CompletionRequests)
	assert.Equal(t, 1, h.client.completeCalls)
	comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Empty(t, comments)
	hasReview, readErr := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, readErr)
	assert.False(t, hasReview)
}

type failExternalRootSuccessOnceStorage struct {
	db.Storage
	err    error
	failed bool
}

func (s *failExternalRootSuccessOnceStorage) RecordExternalRootSuccess(
	ctx context.Context,
	params db.ExternalRootSuccessParams,
) (db.ExternalRootBinding, error) {
	if !s.failed {
		s.failed = true
		return db.ExternalRootBinding{}, s.err
	}
	return s.Storage.RecordExternalRootSuccess(ctx, params)
}

type cancelAfterRootProjectionStorage struct {
	db.Storage
	cancel        context.CancelFunc
	successCtxErr error
}

type cancelAfterFieldProjectionStorage struct {
	db.Storage
	cancel context.CancelFunc
}

type editBeforeFieldProjectionStorage struct {
	db.Storage
	issueID int64
	edited  bool
}

func (s *editBeforeFieldProjectionStorage) ApplyExternalFieldProjection(
	ctx context.Context,
	params db.ExternalFieldProjectionParams,
) (db.Issue, *db.Event, bool, error) {
	if !s.edited {
		s.edited = true
		if _, err := s.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: s.issueID, Actor: "local-operator", Patch: map[string]json.RawMessage{
				"scheduled_on": json.RawMessage(`"2026-08-22"`),
			},
		}); err != nil {
			return db.Issue{}, nil, false, err
		}
	}
	return s.Storage.ApplyExternalFieldProjection(ctx, params)
}

func (s *cancelAfterFieldProjectionStorage) ApplyExternalFieldProjection(
	ctx context.Context,
	params db.ExternalFieldProjectionParams,
) (db.Issue, *db.Event, bool, error) {
	issue, event, changed, err := s.Storage.ApplyExternalFieldProjection(ctx, params)
	if err == nil {
		s.cancel()
	}
	return issue, event, changed, err
}

func (s *cancelAfterRootProjectionStorage) ApplyExternalRootProjection(
	ctx context.Context,
	params db.ExternalRootProjectionParams,
) (db.Issue, *db.Event, bool, error) {
	issue, event, changed, err := s.Storage.ApplyExternalRootProjection(ctx, params)
	if err == nil {
		s.cancel()
	}
	return issue, event, changed, err
}

func (s *cancelAfterRootProjectionStorage) RecordExternalRootSuccess(
	ctx context.Context,
	params db.ExternalRootSuccessParams,
) (db.ExternalRootBinding, error) {
	s.successCtxErr = ctx.Err()
	return s.Storage.RecordExternalRootSuccess(ctx, params)
}

type reconcileHarness struct {
	store      *sqlitestore.Store
	project    db.Project
	issue      db.Issue
	binding    db.ExternalRootBinding
	client     *fakeConnectorClient
	registry   *Registry
	reconciler *Reconciler
	boundAt    time.Time
	now        time.Time
	fields     map[string]db.ExternalFieldMapping
}

func newReconcileHarness(t *testing.T) *reconcileHarness {
	t.Helper()
	return newReconcileHarnessWithOptions(t, "", false)
}

func newReconcileHarnessWithInitialClaim(t *testing.T, initialClaim string) *reconcileHarness {
	t.Helper()
	return newReconcileHarnessWithOptions(t, initialClaim, false)
}

func newPublishingReconcileHarness(t *testing.T) *reconcileHarness {
	t.Helper()
	return newReconcileHarnessWithOptions(t, "", true)
}

func newReconcileHarnessWithOptions(t *testing.T, initialClaim string, publishComments bool) *reconcileHarness {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Local plan", Body: "Local body", Author: "tester",
	})
	require.NoError(t, err)
	boundAt := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	now := boundAt.Add(10 * time.Minute)
	client := &fakeConnectorClient{
		description: testDescription("account-1"),
		read: connector.Root{
			Key: "root-1", IdentityKey: "account-1", Title: "Local plan", Body: "Local body",
			State: "open", Revision: "root-revision-1", UpdatedAt: boundAt, ObservedAt: boundAt,
		},
	}
	if publishComments {
		client.description.Capabilities = []connector.Capability{connector.CapabilityPublishComment}
		client.description.SelfActorID = "connector-self"
	}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	params := db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-1", ExternalAccountKey: "account-1",
		Actor: "tester", ReceiveCommentsAfter: boundAt,
	}
	if publishComments {
		params.PublishComments = true
		params.PublishCommentsAfter = &boundAt
	}
	if initialClaim != "" {
		params.InitialClaimToken = initialClaim
		params.InitialClaimStartedAt = now
	}
	binding, _, err := store.CreateExternalRootBinding(t.Context(), params)
	require.NoError(t, err)
	reconciler := NewReconciler(store, registry, ReconcilerConfig{Now: func() time.Time { return now }})
	return &reconcileHarness{
		store: store, project: project, issue: issue, binding: binding,
		client: client, registry: registry, reconciler: reconciler, boundAt: boundAt, now: now,
		fields: make(map[string]db.ExternalFieldMapping),
	}
}

func (h *reconcileHarness) setAttention(t *testing.T) db.JSONBlob {
	t.Helper()
	out, err := h.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: h.issue.ID, Actor: "tester", Patch: map[string]json.RawMessage{
			"work.attention":     json.RawMessage(`"ok"`),
			"work.attention_msg": json.RawMessage(`"Continue implementation"`),
		},
	})
	require.NoError(t, err)
	h.issue = out.Issue
	return out.Issue.Metadata
}

func (h *reconcileHarness) mapField(t *testing.T, kataField, externalFieldID string) db.ExternalFieldMapping {
	t.Helper()
	return h.mapFieldDescriptor(t, kataField, connector.FieldDescriptor{
		ID: externalFieldID, DisplayName: externalFieldID,
		AcceptedKinds: []string{fieldKindDate, fieldKindLocalDateTime, fieldKindInstant},
		Nullable:      true, Writable: true, SchemaRevision: "schema-1",
	})
}

func (h *reconcileHarness) mapFieldDescriptor(
	t *testing.T,
	kataField string,
	descriptor connector.FieldDescriptor,
) db.ExternalFieldMapping {
	t.Helper()
	hasFields := false
	hasConditionalFields := false
	for _, capability := range h.client.description.Capabilities {
		if capability == connector.CapabilityFields {
			hasFields = true
		}
		if capability == connector.CapabilityConditionalFields {
			hasConditionalFields = true
		}
	}
	if !hasFields {
		h.client.description.Capabilities = append(h.client.description.Capabilities, connector.CapabilityFields)
	}
	if !hasConditionalFields {
		h.client.description.Capabilities = append(h.client.description.Capabilities, connector.CapabilityConditionalFields)
	}
	h.client.fields = append(h.client.fields, descriptor)
	service := NewService(h.store, h.registry, nil)
	mapping, err := service.MapField(t.Context(), MapFieldParams{
		ConnectorInstance: h.binding.ConnectorInstance,
		KataField:         kataField,
		ExternalField:     descriptor.ID,
	})
	require.NoError(t, err)
	h.fields[kataField] = mapping
	if h.client.fieldValues == nil {
		h.client.fieldValues = make(map[string]connector.FieldValue)
	}
	return mapping
}

func (h *reconcileHarness) setKataField(t *testing.T, kataField string, value connector.FieldValue) {
	t.Helper()
	issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	patch, err := fieldCodecs[kataField].KataPatch(issue, value)
	require.NoError(t, err)
	out, err := h.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: h.issue.ID, Actor: "tester", Patch: patch,
	})
	require.NoError(t, err)
	h.issue = out.Issue
}

func (h *reconcileHarness) seedBaseline(
	t *testing.T,
	kataField string,
	value connector.FieldValue,
) db.ExternalFieldState {
	t.Helper()
	mapping, ok := h.fields[kataField]
	require.True(t, ok)
	claimToken := "seed-" + kataField
	_, claimed, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, claimToken, h.now, h.now.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, claimed)
	baseline, err := json.Marshal(value)
	require.NoError(t, err)
	state, event, err := h.store.UpsertExternalFieldState(t.Context(), db.ExternalFieldStateParams{
		BindingID: h.binding.ID, MappingID: mapping.ID, ClaimToken: claimToken,
		Baseline: baseline, At: h.now, Actor: integrationActor(h.binding),
	})
	require.NoError(t, err)
	assert.Nil(t, event)
	_, err = h.store.ReleaseExternalRootClaim(t.Context(), h.binding.ID, claimToken)
	require.NoError(t, err)
	return state
}

func (h *reconcileHarness) fieldState(t *testing.T, kataField string) db.ExternalFieldState {
	t.Helper()
	mapping, ok := h.fields[kataField]
	require.True(t, ok)
	states, err := h.store.ExternalFieldStates(t.Context(), h.binding.ID)
	require.NoError(t, err)
	for _, state := range states {
		if state.MappingID == mapping.ID {
			return state
		}
	}
	require.FailNow(t, "field state missing", "field state for %s was not stored", kataField)
	return db.ExternalFieldState{}
}

func (h *reconcileHarness) requireSuccessfulClaim(t *testing.T, state, revision string) {
	t.Helper()
	binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	assert.Empty(t, binding.ClaimToken)
	assert.NotNil(t, binding.LastSuccessAt)
	assert.Equal(t, state, binding.LastExternalState)
	assert.Equal(t, revision, binding.LastExternalRevision)
}

func requireEventType(t *testing.T, events []db.Event, eventType string) db.Event {
	t.Helper()
	for _, event := range events {
		if event.Type == eventType {
			return event
		}
	}
	require.FailNow(t, "event type missing", "event type %q not found in %v", eventType, eventTypes(events))
	return db.Event{}
}

func eventTypes(events []db.Event) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func decodeStoredFieldValue(t *testing.T, raw json.RawMessage) connector.FieldValue {
	t.Helper()
	var value connector.FieldValue
	require.NoError(t, json.Unmarshal(raw, &value))
	return value
}

func containsEventType(events []db.Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}
