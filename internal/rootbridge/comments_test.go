package rootbridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/connector"
)

func TestPublicationOperationIDIsStableForBindingAndComment(t *testing.T) {
	binding := db.ExternalRootBinding{UID: "01JEXAMPLEBINDING000000000"}
	comment := db.Comment{UID: "01JEXAMPLECOMMENT000000000"}

	first := publicationOperationID(binding, comment)
	second := publicationOperationID(binding, comment)

	assert.NotEmpty(t, first)
	assert.Equal(t, first, second)
	assert.NotEqual(t, first, publicationOperationID(
		db.ExternalRootBinding{UID: "01JOTHERBINDING0000000000"}, comment,
	))
	assert.NotEqual(t, first, publicationOperationID(
		binding, db.Comment{UID: "01JOTHERCOMMENT0000000000"},
	))
}

func TestReconcileQuietModeDoesNotPublishLocalComments(t *testing.T) {
	h := newReconcileHarness(t)
	local := h.createLocalComment(t, "Keep this local")

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, h.client.publishCalls)
	binding := h.requireBinding(t)
	assert.Empty(t, binding.PendingCommentUID)
	assertNoCommentMapping(t, h.store, h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), local.ID)
}

func TestReconcilePublishesEligibleCommentExactlyOnceWithoutMarkers(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Ship this exact body\n\nwithout a marker")
	h.client.read.Title = "External title applied first"
	h.client.read.Revision = "root-revision-2"
	h.client.read.UpdatedAt = h.now
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	h.client.beforePublish = func() {
		issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
		require.NoError(t, err)
		assert.Equal(t, "External title applied first", issue.Title)
	}
	createdAt := h.now.Add(time.Minute)
	h.client.publishResult = connector.Comment{
		ID: "external-comment-9", Revision: "published-revision-9", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Len(t, h.client.publishParams, 1)
	assert.Equal(t, "root-1", h.client.publishParams[0].RootKey)
	assert.Equal(t, local.Body, h.client.publishParams[0].Body)
	assert.NotEmpty(t, h.client.publishParams[0].OperationID)
	assert.NotContains(t, h.client.publishParams[0].OperationID, local.UID)
	assert.NotContains(t, h.client.publishParams[0].OperationID, h.binding.UID)
	binding := h.requireBinding(t)
	assert.Empty(t, binding.PendingCommentUID)
	mapping := requireCommentMapping(t, h.store, h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), "external-comment-9")
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, local.ID, *mapping.CommentID)
	assert.Contains(t, eventTypes(first.Events), "issue.external_comment_resolved")

	_, _, _, err = h.store.EditComment(t.Context(), db.EditCommentParams{
		IssueID: h.issue.ID, CommentUID: local.UID, Actor: "tester", Body: "Edited after delivery",
	})
	require.NoError(t, err)
	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, h.client.publishCalls)
}

func TestReconcileExternalEditOfPublishedCommentPreservesLocalAuthorship(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createComment(t, h.issue.ID, "local-operator", "Original local body")
	published := publishedComment(h, "published-comment", local.Body)
	h.client.publishResult = published

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	h.client.comments = []connector.Comment{published}
	unchanged, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, unchanged.CommentsCreated)
	assert.Zero(t, unchanged.CommentsEdited)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)

	published.Body = "Edited by an external collaborator"
	published.Revision = "published-comment-revision-2"
	published.Author = connector.Actor{ID: "external-collaborator", DisplayName: "External collaborator"}
	published.UpdatedAt = published.UpdatedAt.Add(time.Minute)
	h.client.comments = []connector.Comment{published}

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.CommentsCreated)
	assert.Zero(t, result.CommentsEdited)
	comments, err = h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	byID := make(map[int64]db.Comment, len(comments))
	for _, comment := range comments {
		byID[comment.ID] = comment
	}
	assert.Equal(t, "local-operator", byID[local.ID].Author)
	assert.Equal(t, "Original local body", byID[local.ID].Body)
	for _, comment := range comments {
		if comment.ID != local.ID {
			assert.Equal(t, "connector:notes", comment.Author)
			assert.Equal(t, "Edited by an external collaborator", comment.Body)
		}
	}
}

func TestReconcilePublishesUnmappedLocalActorWithConnectorPrefix(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createComment(t, h.issue.ID, "connector:local-operator", "Locally authored body")
	h.client.publishResult = publishedComment(h, "published-prefixed-actor", local.Body)

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, h.client.publishCalls)
	assert.Contains(t, eventTypes(result.Events), "issue.external_comment_resolved")
}

func TestReconcileRejectsMalformedPublishedCommentAndKeepsPending(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*reconcileHarness, *connector.Comment)
	}{
		{name: "missing ID", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.ID = ""
		}},
		{name: "missing revision", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.Revision = ""
		}},
		{name: "body mismatch", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.Body = "Different provider body"
		}},
		{name: "wrong author", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.Author.ID = "different-actor"
		}},
		{name: "deleted", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.Deleted = true
		}},
		{name: "missing created timestamp", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.CreatedAt = time.Time{}
		}},
		{name: "missing updated timestamp", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.UpdatedAt = time.Time{}
		}},
		{name: "updated before creation", mutate: func(_ *reconcileHarness, comment *connector.Comment) {
			comment.UpdatedAt = comment.CreatedAt.Add(-time.Second)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPublishingReconcileHarness(t)
			local := h.createLocalComment(t, "Publish exactly this body")
			published := publishedComment(h, "external-malformed", local.Body)
			test.mutate(h, &published)
			h.client.publishResult = published

			_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

			require.Error(t, err)
			assert.ErrorIs(t, err, connectorclient.ErrProtocolFailure)
			assert.Equal(t, connectorclient.ErrProtocolFailure.Error(), err.Error())
			binding := h.requireBinding(t)
			assert.Equal(t, local.UID, binding.PendingCommentUID)
			assert.Equal(t, connectorclient.ErrProtocolFailure.Error(), binding.LastError)
			assertNoCommentMapping(
				t, h.store, h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), local.ID,
			)
		})
	}
}

func TestReconcileAcceptsCoarsePublishedCommentTimestamp(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Publish through a coarse-clock provider")
	published := publishedComment(h, "external-coarse-timestamp", local.Body)
	published.CreatedAt = h.now.Add(-time.Minute).Truncate(time.Minute)
	published.UpdatedAt = published.CreatedAt
	h.client.publishResult = published

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Empty(t, h.requireBinding(t).PendingCommentUID)
	mapping := requireCommentMapping(
		t, h.store, h.project.ID,
		db.ExternalRootPublishedCommentMappingSource(h.binding), published.ID,
	)
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, local.ID, *mapping.CommentID)
}

func TestReconcilePublishesSameProviderCommentIDForDifferentRoots(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	firstLocal := h.createLocalComment(t, "First root comment")
	h.client.publishResult = publishedComment(h, "root-local-comment-id", firstLocal.Body)

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)

	secondIssue, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.project.ID, Title: "Second local plan", Body: "Second local body", Author: "tester",
	})
	require.NoError(t, err)
	secondBinding, _, err := h.store.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: h.project.ID, IssueID: secondIssue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-2", ExternalAccountKey: "account-1",
		Actor: "tester", ReceiveCommentsAfter: h.boundAt,
		PublishComments: true, PublishCommentsAfter: &h.boundAt,
	})
	require.NoError(t, err)
	secondLocal := h.createComment(t, secondIssue.ID, "tester", "Second root comment")
	h.client.read = connector.Root{
		Key: "root-2", IdentityKey: "account-1", Title: secondIssue.Title, Body: secondIssue.Body,
		State: "open", Revision: "root-2-revision-1", UpdatedAt: h.now, ObservedAt: h.now,
	}
	h.client.publishResult = publishedComment(h, "root-local-comment-id", secondLocal.Body)

	_, err = h.reconciler.Run(t.Context(), secondBinding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, h.client.publishCalls)

	for _, item := range []struct {
		binding db.ExternalRootBinding
		comment db.Comment
	}{
		{binding: h.binding, comment: firstLocal},
		{binding: secondBinding, comment: secondLocal},
	} {
		mapping := requireCommentMapping(
			t, h.store, h.project.ID,
			db.ExternalRootPublishedCommentMappingSource(item.binding),
			"root-local-comment-id",
		)
		require.NotNil(t, mapping.CommentID)
		assert.Equal(t, item.comment.ID, *mapping.CommentID)
	}
}

func TestReconcilePublishesOnlyUnmappedRootCommentsFromFrontier(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	eligible := h.createLocalComment(t, "Eligible root comment")
	imported := h.createLocalComment(t, "Imported comment")
	child, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.project.ID, Title: "Child work", Author: "tester",
	})
	require.NoError(t, err)
	_, err = h.store.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: h.issue.ID, Type: "parent", Author: "tester",
	})
	require.NoError(t, err)
	childComment := h.createComment(t, child.ID, "tester", "Child comment")
	issueID, importedID := h.issue.ID, imported.ID
	_, err = h.store.UpsertImportMapping(t.Context(), db.ImportMappingParams{
		Source: "import:fixture", ExternalID: "imported-comment", ObjectType: "comment",
		ProjectID: h.project.ID, IssueID: &issueID, CommentID: &importedID,
	})
	require.NoError(t, err)
	createdAt := h.now.Add(time.Minute)
	h.client.publishResult = connector.Comment{
		ID: "external-eligible", Revision: "revision-external-eligible", Body: eligible.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Len(t, h.client.publishParams, 1)
	assert.Equal(t, eligible.Body, h.client.publishParams[0].Body)
	assertNoCommentMapping(t, h.store, h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), childComment.ID)
}

func TestReconcilePublishingRequiresCurrentCapabilityAndSelfActor(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*connector.Description)
	}{
		{name: "capability removed", mutate: func(description *connector.Description) {
			description.Capabilities = nil
		}},
		{name: "self actor removed", mutate: func(description *connector.Description) {
			description.SelfActorID = ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPublishingReconcileHarness(t)
			local := h.createLocalComment(t, "Retain locally")
			h.client.read.Title = "Inbound title remains available"
			h.client.read.State = "complete"
			h.client.read.Revision = "root-revision-inbound"
			h.client.read.UpdatedAt = h.now
			h.client.read.ObservedAt = h.client.read.UpdatedAt
			providerTime := h.boundAt.Add(time.Minute)
			h.client.comments = []connector.Comment{{
				ID: "inbound-comment", Body: "Inbound comment remains available",
				Author:    connector.Actor{ID: "external-author", DisplayName: "Contributor"},
				CreatedAt: providerTime, UpdatedAt: providerTime,
			}}
			test.mutate(&h.client.description)

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
			assert.ErrorIs(t, err, ErrCommentPublishingUnavailable)
			assert.Zero(t, h.client.publishCalls)
			assert.True(t, result.RootUpdated)
			assert.Equal(t, 1, result.CommentsCreated)
			assert.Equal(t, 1, result.CompletionRequests)
			assert.Contains(t, eventTypes(result.Events), "issue.updated")
			assert.Contains(t, eventTypes(result.Events), "issue.commented")
			assert.Contains(t, eventTypes(result.Events), "issue.labeled")
			issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.Equal(t, "Inbound title remains available", issue.Title)
			mapping := requireCommentMapping(
				t, h.store, h.project.ID, db.ExternalRootCommentMappingSource(h.binding), "inbound-comment",
			)
			require.NotNil(t, mapping.CommentID)
			comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.Len(t, comments, 3)
			hasReview, readErr := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
			require.NoError(t, readErr)
			assert.True(t, hasReview)
			assert.Empty(t, h.requireBinding(t).PendingCommentUID)
			assertNoCommentMapping(
				t, h.store, h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), local.ID,
			)
		})
	}
}

func TestCompleteExternalWaitsForOutboundPublication(t *testing.T) {
	for _, test := range []struct {
		name          string
		configure     func(*reconcileHarness) []error
		wantPublishes int
	}{
		{
			name: "publishing capability unavailable",
			configure: func(h *reconcileHarness) []error {
				h.client.description.Capabilities = nil
				return []error{ErrCommentPublishingUnavailable}
			},
		},
		{
			name: "publish transport failure",
			configure: func(h *reconcileHarness) []error {
				publishErr := &connector.Error{Code: "temporarily_unavailable", Message: "publish unavailable"}
				h.client.publishErr = publishErr
				return []error{publishErr}
			},
			wantPublishes: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPublishingReconcileHarness(t)
			h.createLocalComment(t, "Attempt before completion")
			_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
			require.NoError(t, err)
			h.client.read.Title = "Projected before outbound failure"
			h.client.read.Revision = "root-before-outbound-failure"
			h.client.read.UpdatedAt = h.now
			h.client.read.ObservedAt = h.client.read.UpdatedAt
			h.client.completeReadback = completedRoot(h.client.read, "completed-after-outbound-failure")
			wantErrors := test.configure(h)

			result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

			for _, wantErr := range wantErrors {
				assert.ErrorIs(t, err, wantErr)
			}
			assert.Zero(t, h.client.completeCalls)
			assert.Equal(t, test.wantPublishes, h.client.publishCalls)
			assert.Contains(t, eventTypes(result.Events), "issue.updated")
			binding := h.requireBinding(t)
			assert.Nil(t, binding.LastSuccessAt)
			assert.Equal(t, 1, binding.ConsecutiveFailures)
			assert.Empty(t, binding.ClaimToken)
		})
	}
}

func TestCompleteExternalWaitsForPendingReplay(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Pending before completion")
	claimed, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, "pending-before-completion", h.now, h.now.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = h.store.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{
		BindingID: h.binding.ID, ClaimToken: claimed.ClaimToken, CommentUID: local.UID, At: h.now,
	})
	require.NoError(t, err)
	_, err = h.store.RecordExternalRootError(t.Context(), db.ExternalRootErrorParams{
		BindingID: h.binding.ID, ClaimToken: claimed.ClaimToken,
		At: h.now, NextAttemptAt: h.now, Error: "publication result uncertain",
	})
	require.NoError(t, err)
	_, _, _, err = h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.read.Title = "Projected before pending resolution"
	h.client.read.Revision = "root-before-pending-resolution"
	h.client.read.UpdatedAt = h.now
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	h.client.completeReadback = completedRoot(h.client.read, "completed-with-pending-comment")
	publishErr := &connector.Error{Code: "temporarily_unavailable", Message: "publish unavailable"}
	h.client.publishErr = publishErr

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	assert.ErrorIs(t, err, publishErr)
	assert.Zero(t, h.client.completeCalls)
	assert.Equal(t, 1, h.client.publishCalls)
	assert.Contains(t, eventTypes(result.Events), "issue.updated")
	binding := h.requireBinding(t)
	assert.Equal(t, local.UID, binding.PendingCommentUID)
	assert.Nil(t, binding.LastSuccessAt)
	assert.Equal(t, 2, binding.ConsecutiveFailures)
	assert.Empty(t, binding.ClaimToken)
}

func TestCompletionRunsAfterOutboundRetrySucceeds(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Publish after completion retry")
	_, _, _, err := h.store.CloseIssue(t.Context(), h.issue.ID, "done", "verifier", "", nil)
	require.NoError(t, err)
	h.client.completeReadback = completedRoot(h.client.read, "completed-during-outbound-error")
	h.client.description.Capabilities = nil

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	assert.ErrorIs(t, err, ErrCommentPublishingUnavailable)
	assert.Zero(t, first.CompletionRequests)
	assert.Zero(t, first.ReopenRequests)
	assert.Zero(t, h.client.completeCalls)
	failed := h.requireBinding(t)
	assert.Nil(t, failed.LastSuccessAt)
	assert.Equal(t, 1, failed.ConsecutiveFailures)
	assert.Empty(t, failed.LastExternalState)
	assert.Empty(t, failed.LastExternalRevision)

	h.client.description.Capabilities = []connector.Capability{connector.CapabilityPublishComment}
	h.client.description.SelfActorID = "connector-self"
	h.client.publishResult = publishedComment(h, "published-after-completion-retry", local.Body)
	second, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, second.CompletionRequests)
	assert.Zero(t, second.ReopenRequests)
	assert.Equal(t, 1, h.client.completeCalls)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 1)
	hasReview, err := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, err)
	assert.False(t, hasReview)
	succeeded := h.requireBinding(t)
	require.NotNil(t, succeeded.LastSuccessAt)
	assert.Equal(t, "complete", succeeded.LastExternalState)
	assert.Equal(t, "completed-during-outbound-error", succeeded.LastExternalRevision)

	h.client.read.State = "open"
	h.client.read.Revision = "reopened-after-completion"
	h.client.read.UpdatedAt = h.client.read.UpdatedAt.Add(time.Minute)
	h.client.read.ObservedAt = h.client.read.ObservedAt.Add(time.Minute)
	h.client.completeReadback = completedRoot(h.client.read, "recompleted-after-reopen")
	third, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	require.NoError(t, err)
	assert.Zero(t, third.CompletionRequests)
	assert.Equal(t, 1, third.ReopenRequests)
	hasReview, err = h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, err)
	assert.True(t, hasReview)
}

func TestReconcileWithoutSelfIdentityWithholdsPlausiblePendingCommentFromInbound(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Uncertain external creation")
	claimed, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, "pending-setup-claim", h.now, h.now.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = h.store.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{
		BindingID: h.binding.ID, ClaimToken: claimed.ClaimToken, CommentUID: local.UID, At: h.now,
	})
	require.NoError(t, err)
	_, err = h.store.RecordExternalRootError(t.Context(), db.ExternalRootErrorParams{
		BindingID: h.binding.ID, ClaimToken: claimed.ClaimToken,
		At: h.now, NextAttemptAt: h.now, Error: "publication result uncertain",
	})
	require.NoError(t, err)
	h.client.description.SelfActorID = ""
	h.client.read.Title = "Inbound root still applies"
	h.client.read.Revision = "root-revision-after-pending"
	h.client.read.UpdatedAt = h.now.Add(time.Minute)
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	h.client.comments = []connector.Comment{{
		ID: "plausible-pending-comment", Body: local.Body,
		Author:    connector.Actor{ID: "unknown-current-actor"},
		CreatedAt: h.now.Add(time.Minute), UpdatedAt: h.now.Add(time.Minute),
	}}

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	assert.ErrorIs(t, err, ErrCommentPublishingUnavailable)
	assert.ErrorIs(t, err, ErrPendingCommentResolutionRequired)
	assert.True(t, result.RootUpdated)
	assert.Zero(t, result.CommentsCreated)
	assert.Zero(t, h.client.publishCalls)
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "Inbound root still applies", issue.Title)
	comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	require.Len(t, comments, 1)
	assert.Equal(t, local.ID, comments[0].ID)
	_, readErr = h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootCommentMappingSource(h.binding), "comment", "plausible-pending-comment",
	)
	assert.ErrorIs(t, readErr, db.ErrNotFound)
	retained := h.requireBinding(t)
	assert.Equal(t, local.UID, retained.PendingCommentUID)
	assert.Empty(t, retained.ClaimToken)
}

func TestResolvePendingCommentRejectsBlankSelfIdentityAtAutomaticAdoptionBoundary(t *testing.T) {
	for _, selfActorID := range []string{"", "   "} {
		name := "empty"
		if selfActorID != "" {
			name = "whitespace"
		}
		t.Run(name, func(t *testing.T) {
			h := newPublishingReconcileHarness(t)
			local := h.createLocalComment(t, "Withhold this plausible uncertain create")
			claimed, acquired, err := h.store.ClaimExternalRootBinding(
				t.Context(), h.binding.ID, "direct-pending-claim", h.now, h.now.Add(-time.Minute),
			)
			require.NoError(t, err)
			require.True(t, acquired)
			_, err = h.store.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{
				BindingID: h.binding.ID, ClaimToken: claimed.ClaimToken, CommentUID: local.UID, At: h.now,
			})
			require.NoError(t, err)
			h.client.description.SelfActorID = selfActorID
			h.client.comments = []connector.Comment{{
				ID: "plausible-without-self", Body: local.Body,
				Author:    connector.Actor{ID: "unaccepted-actor"},
				CreatedAt: h.now, UpdatedAt: h.now,
			}}
			snapshot, err := h.reconciler.readCurrent(t.Context(), claimed, claimed.ClaimToken)
			require.NoError(t, err)

			result, err := h.reconciler.resolvePendingComment(
				t.Context(), snapshot, claimed.ClaimToken, RunResult{},
			)
			assert.ErrorIs(t, err, ErrPendingCommentResolutionRequired)
			assert.Empty(t, result.Events)
			assert.Zero(t, h.client.publishCalls)
			_, mappingErr := h.store.ImportMappingBySource(
				t.Context(), h.project.ID, db.ExternalRootCommentMappingSource(h.binding), "comment", "plausible-without-self",
			)
			assert.ErrorIs(t, mappingErr, db.ErrNotFound)
			retained := h.requireBinding(t)
			assert.Equal(t, local.UID, retained.PendingCommentUID)

			_, outboundErr := h.reconciler.applyOutboundComments(
				t.Context(), snapshot, claimed.ClaimToken, RunResult{},
			)
			assert.ErrorIs(t, outboundErr, ErrCommentPublishingUnavailable)
			assert.ErrorIs(t, outboundErr, ErrPendingCommentResolutionRequired)
			assert.Zero(t, h.client.publishCalls)
		})
	}
}

func TestBindPublishingUsesDurableLocalCommentIdentityFrontier(t *testing.T) {
	h := newServiceHarness(t)
	old := h.createServiceComment(t, "Existing local comment")
	h.client.read.UpdatedAt = testObservedAt
	reconciler := NewReconciler(h.observed, h.registry, ReconcilerConfig{})
	h.service.immediateClaimedReconcile = func(ctx context.Context, bindingID int64, claimToken string) ([]db.Event, error) {
		result, err := reconciler.Run(ctx, bindingID, RunOptions{ClaimToken: claimToken})
		return result.Events, err
	}

	binding, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester", PublishComments: true,
	})
	require.NoError(t, err)
	require.NotNil(t, binding.PublishCommentsAfter)
	after := h.createServiceComment(t, "Created after publishing was enabled")
	_, err = h.store.ExecContext(t.Context(), `UPDATE comments SET created_at=? WHERE id=?`,
		old.CreatedAt.Add(-time.Hour).Format(time.RFC3339Nano), after.ID)
	require.NoError(t, err)
	h.client.publishResult = connector.Comment{
		ID: "external-after-enable", Revision: "revision-external-after-enable", Body: after.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: binding.PublishCommentsAfter.Add(time.Minute), UpdatedAt: binding.PublishCommentsAfter.Add(time.Minute),
	}

	_, err = reconciler.Run(t.Context(), binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Len(t, h.client.publishParams, 1)
	assert.Equal(t, after.Body, h.client.publishParams[0].Body)
	assertNoCommentMapping(t, h.store, h.project.ID, db.ExternalRootPublishedCommentMappingSource(binding), old.ID)
}

func TestPublishMappingFailureRetriesSameOperationAndResolves(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Ship it")
	h.client.read.Title = "Committed before publication"
	h.client.read.Revision = "root-revision-2"
	h.client.read.UpdatedAt = h.now
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	h.client.publishResult = publishedComment(h, "external-comment-9", local.Body)
	postCreateErr := errors.New("durable mapping unavailable")
	h.reconciler.store = &failPublishedCommentClearOnceStorage{Storage: h.store, err: postCreateErr}

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.ErrorIs(t, err, postCreateErr)
	assert.Contains(t, eventTypes(result.Events), "issue.updated")
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, "Committed before publication", issue.Title)
	result, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Len(t, h.client.publishParams, 2)
	assert.Equal(t, h.client.publishParams[0].OperationID, h.client.publishParams[1].OperationID)
	assert.Empty(t, h.requireBinding(t).PendingCommentUID)
	mapping := requireCommentMapping(t, h.store, h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding), "external-comment-9")
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, local.ID, *mapping.CommentID)
	assert.Contains(t, eventTypes(result.Events), "issue.external_comment_resolved")
}

func TestPendingReplayDoesNotUseListCommentTimestampMatches(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Recover through the publication operation")
	original := publishedComment(h, "external-comment-original", local.Body)
	h.client.publishResult = original
	h.reconciler.store = &failPublishedCommentClearOnceStorage{
		Storage: h.store, err: errors.New("durable mapping unavailable"),
	}
	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.Error(t, err)
	listed := publishedComment(h, "unrelated-exact-body-comment", local.Body)
	listed.CreatedAt = h.now.Add(-24 * time.Hour).Truncate(time.Minute)
	listed.UpdatedAt = listed.CreatedAt
	h.client.comments = []connector.Comment{listed}

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, h.client.publishCalls)
	assert.Empty(t, h.requireBinding(t).PendingCommentUID)
	mapping := requireCommentMapping(
		t, h.store, h.project.ID,
		db.ExternalRootPublishedCommentMappingSource(h.binding), original.ID,
	)
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, local.ID, *mapping.CommentID)
	comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Len(t, comments, 1)
}

func TestPendingReplayRejectsMismatchedConnectorResult(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Recover only the original publication")
	h.client.publishResult = publishedComment(h, "external-comment-original", local.Body)
	h.reconciler.store = &failPublishedCommentClearOnceStorage{
		Storage: h.store, err: errors.New("durable mapping unavailable"),
	}
	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.Error(t, err)
	h.client.publishResult = publishedComment(h, "external-comment-mismatch", "Different provider body")

	_, err = h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	assert.ErrorIs(t, err, connectorclient.ErrProtocolFailure)
	assert.Equal(t, 2, h.client.publishCalls)
	assert.Equal(t, local.UID, h.requireBinding(t).PendingCommentUID)
}

func TestConcurrentReconcileWakesPublishOnce(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Publish once")
	h.client.publishResult = publishedComment(h, "external-comment-13", local.Body)
	entered := make(chan struct{})
	release := make(chan struct{})
	h.client.beforePublish = func() { close(entered); <-release }

	type runResult struct{ err error }
	results := make(chan runResult, 2)
	go func() {
		_, err := h.reconciler.Run(context.Background(), h.binding.ID, RunOptions{})
		results <- runResult{err: err}
	}()
	<-entered
	go func() {
		_, err := h.reconciler.Run(context.Background(), h.binding.ID, RunOptions{})
		results <- runResult{err: err}
	}()
	second := <-results
	require.NoError(t, second.err)
	close(release)
	first := <-results
	require.NoError(t, first.err)
	assert.Equal(t, 1, h.client.publishCalls)
}

func TestOutboundCommentEditDuringPublicationLeavesResolutionPending(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Body selected for publication")
	h.client.publishResult = publishedComment(h, "external-comment-edited-race", local.Body)
	entered := make(chan struct{})
	release := make(chan struct{})
	h.client.beforePublish = func() {
		close(entered)
		<-release
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.reconciler.Run(context.Background(), h.binding.ID, RunOptions{})
		done <- err
	}()
	<-entered
	_, _, changed, err := h.store.EditComment(t.Context(), db.EditCommentParams{
		IssueID: h.issue.ID, CommentUID: local.UID, Body: "Edited while publishing", Actor: "operator",
	})
	require.NoError(t, err)
	require.True(t, changed)
	close(release)

	require.ErrorIs(t, <-done, db.ErrExternalRootValidation)
	retained := h.requireBinding(t)
	assert.Equal(t, local.UID, retained.PendingCommentUID)
	_, err = h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootPublishedCommentMappingSource(h.binding),
		"comment", "external-comment-edited-race",
	)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

type failPublishedCommentClearOnceStorage struct {
	db.Storage
	err  error
	once sync.Once
}

func (s *failPublishedCommentClearOnceStorage) ClearPendingExternalComment(
	ctx context.Context,
	params db.ClearPendingExternalCommentParams,
) (db.ExternalRootBinding, db.Event, error) {
	failed := false
	if params.Action == "published" {
		s.once.Do(func() { failed = true })
	}
	if failed {
		return db.ExternalRootBinding{}, db.Event{}, s.err
	}
	return s.Storage.ClearPendingExternalComment(ctx, params)
}

func (h *reconcileHarness) createLocalComment(t *testing.T, body string) db.Comment {
	t.Helper()
	return h.createComment(t, h.issue.ID, "tester", body)
}

func (h *reconcileHarness) createComment(t *testing.T, issueID int64, author, body string) db.Comment {
	t.Helper()
	comment, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issueID, Author: author, Body: body,
	})
	require.NoError(t, err)
	return comment
}

func (h *serviceHarness) createServiceComment(t *testing.T, body string) db.Comment {
	t.Helper()
	comment, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: h.issue.ID, Author: "tester", Body: body,
	})
	require.NoError(t, err)
	return comment
}

func (h *reconcileHarness) requireBinding(t *testing.T) db.ExternalRootBinding {
	t.Helper()
	binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	return binding
}

func publishedComment(h *reconcileHarness, externalID, body string) connector.Comment {
	createdAt := h.now.Add(time.Minute)
	return connector.Comment{
		ID: externalID, Revision: "revision-" + externalID, Body: body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

func requireCommentMapping(
	t *testing.T,
	store db.Storage,
	projectID int64,
	source string,
	externalID string,
) db.ImportMapping {
	t.Helper()
	mapping, err := store.ImportMappingBySource(t.Context(), projectID, source, "comment", externalID)
	require.NoError(t, err)
	return mapping
}

func assertNoCommentMapping(t *testing.T, store db.Storage, projectID int64, source string, commentID int64) {
	t.Helper()
	for mapping, err := range store.ExportImportMappings(t.Context(), db.ExportFilter{ProjectID: &projectID}) {
		require.NoError(t, err)
		if mapping.Source == source && mapping.ObjectType == "comment" && mapping.CommentID != nil {
			assert.NotEqual(t, commentID, *mapping.CommentID)
		}
	}
}

func TestReconcileInboundCommentIsNativeDeduplicatedAndAttributed(t *testing.T) {
	h := newReconcileHarness(t)
	createdAt := h.boundAt.Add(time.Minute)
	h.client.comments = []connector.Comment{{
		ID: "comment-7", Revision: "opaque-comment-revision-7", Body: "Please check this",
		Author:    connector.Actor{ID: "actor-4", DisplayName: "local-operator"},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}}

	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	second, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, first.CommentsCreated)
	assert.Equal(t, 0, second.CommentsCreated)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "connector:notes", comments[0].Author)
	assert.Equal(t, createdAt, comments[0].CreatedAt)

	event := requireEventType(t, first.Events, "issue.commented")
	assert.Equal(t, "connector:notes", event.Actor)
	var payload struct {
		Author string `json:"author"`
		Source struct {
			ConnectorInstance string `json:"connector_instance"`
			ExternalCommentID string `json:"external_comment_id"`
			ExternalRevision  string `json:"external_revision"`
			ActorID           string `json:"actor_id"`
			ActorName         string `json:"actor_name"`
			CreatedAt         string `json:"created_at"`
			UpdatedAt         string `json:"updated_at"`
		} `json:"source"`
	}
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	assert.Equal(t, "connector:notes", payload.Author)
	assert.Equal(t, "notes", payload.Source.ConnectorInstance)
	assert.Equal(t, "comment-7", payload.Source.ExternalCommentID)
	assert.Equal(t, "opaque-comment-revision-7", payload.Source.ExternalRevision)
	assert.Equal(t, "actor-4", payload.Source.ActorID)
	assert.Equal(t, "local-operator", payload.Source.ActorName)
	assert.Equal(t, createdAt.Format(time.RFC3339Nano), payload.Source.CreatedAt)
	assert.Equal(t, createdAt.Format(time.RFC3339Nano), payload.Source.UpdatedAt)
}

func TestReconcileRejectsBlankLiveExternalComment(t *testing.T) {
	h := newReconcileHarness(t)
	createdAt := h.boundAt.Add(time.Minute)
	h.client.comments = []connector.Comment{{
		ID: "comment-blank", Revision: "comment-blank-revision-1", Body: " \t\n",
		Author:    connector.Actor{ID: "actor-4", DisplayName: "Contributor"},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}}

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})

	assert.ErrorIs(t, err, connectorclient.ErrProtocolFailure)
	comments, readErr := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Empty(t, comments)
}

func TestReconcileImportsSameProviderCommentIDForDifferentRoots(t *testing.T) {
	h := newReconcileHarness(t)
	providerTime := h.boundAt.Add(time.Minute)
	h.client.comments = []connector.Comment{{
		ID: "root-local-comment-id", Body: "First root comment",
		Author:    connector.Actor{ID: "actor-1", DisplayName: "Contributor"},
		CreatedAt: providerTime, UpdatedAt: providerTime,
	}}

	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)

	secondIssue, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.project.ID, Title: "Second local plan", Body: "Second local body", Author: "tester",
	})
	require.NoError(t, err)
	secondBinding, _, err := h.store.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: h.project.ID, IssueID: secondIssue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-2", ExternalAccountKey: "account-1",
		Actor: "tester", ReceiveCommentsAfter: h.boundAt,
	})
	require.NoError(t, err)
	h.client.read = connector.Root{
		Key: "root-2", IdentityKey: "account-1", Title: secondIssue.Title, Body: secondIssue.Body,
		State: "open", Revision: "root-2-revision-1", UpdatedAt: h.now, ObservedAt: h.now,
	}
	h.client.comments = []connector.Comment{{
		ID: "root-local-comment-id", Body: "Second root comment",
		Author:    connector.Actor{ID: "actor-2", DisplayName: "Reviewer"},
		CreatedAt: providerTime, UpdatedAt: providerTime,
	}}

	second, err := h.reconciler.Run(t.Context(), secondBinding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, second.CommentsCreated)
	comments, err := h.store.CommentsByIssue(t.Context(), secondIssue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "Second root comment", comments[0].Body)
}

func TestReconcileInboundCommentEditAndDeletionRedactInPlace(t *testing.T) {
	h := newReconcileHarness(t)
	createdAt := h.boundAt.Add(time.Minute)
	h.client.comments = []connector.Comment{{
		ID: "comment-8", Revision: "comment-8-revision-1", Body: "Initial body",
		Author:    connector.Actor{ID: "actor-5", DisplayName: "Contributor"},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}}
	first, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, first.CommentsCreated)

	h.client.comments[0].Body = "Corrected body"
	h.client.comments[0].Revision = "comment-8-revision-2"
	edited, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, edited.CommentsEdited)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "Corrected body", comments[0].Body)
	editEvent := requireEventType(t, edited.Events, "issue.comment_edited")
	var editPayload inboundCommentEventPayload
	require.NoError(t, json.Unmarshal([]byte(editEvent.Payload), &editPayload))
	assert.Equal(t, "actor-5", editPayload.Source.ActorID)
	assert.Equal(t, "Contributor", editPayload.Source.ActorName)
	assert.Equal(t, createdAt.Format(time.RFC3339Nano), editPayload.Source.CreatedAt)
	assert.Equal(t, createdAt.Format(time.RFC3339Nano), editPayload.Source.UpdatedAt)
	assert.False(t, editPayload.Source.Deleted)

	h.client.comments[0].Deleted = true
	h.client.comments[0].Revision = "comment-8-revision-3"
	redacted, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, redacted.CommentsEdited)
	comments, err = h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "[deleted externally]", comments[0].Body)
	deleteEvent := requireEventType(t, redacted.Events, "issue.comment_edited")
	var deletePayload inboundCommentEventPayload
	require.NoError(t, json.Unmarshal([]byte(deleteEvent.Payload), &deletePayload))
	assert.Equal(t, "actor-5", deletePayload.Source.ActorID)
	assert.Equal(t, "Contributor", deletePayload.Source.ActorName)
	assert.Equal(t, createdAt.Format(time.RFC3339Nano), deletePayload.Source.CreatedAt)
	assert.Equal(t, createdAt.Format(time.RFC3339Nano), deletePayload.Source.UpdatedAt)
	assert.True(t, deletePayload.Source.Deleted)
	mapping, err := h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootCommentMappingSource(h.binding), "comment", "comment-8",
	)
	require.NoError(t, err)
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, comments[0].ID, *mapping.CommentID)
}

type inboundCommentEventPayload struct {
	Source struct {
		ActorID   string `json:"actor_id"`
		ActorName string `json:"actor_name"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		Deleted   bool   `json:"deleted"`
	} `json:"source"`
}

func TestReconcileExcludesCommentsAtOrBeforeBindFrontier(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.comments = []connector.Comment{
		{ID: "comment-before", Body: "Earlier", Author: connector.Actor{DisplayName: "Reviewer"}, CreatedAt: h.boundAt.Add(-time.Second), UpdatedAt: h.boundAt.Add(time.Minute)},
		{ID: "comment-at", Body: "At frontier", Author: connector.Actor{DisplayName: "Reviewer"}, CreatedAt: h.boundAt, UpdatedAt: h.boundAt},
	}

	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, result.CommentsCreated)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Empty(t, comments)
}
