package rootbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	katauid "go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/connector"
)

var testObservedAt = time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

func TestBindResolvesExistingRootWithoutExternalMutation(t *testing.T) {
	h := newServiceHarness(t)

	got, events, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "https://external.example/root/1", Actor: "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "root-1", got.ExternalRootKey)
	assert.Equal(t, &testObservedAt, got.ReceiveCommentsAfter)
	assert.Len(t, events, 1)
	assert.Equal(t, 1, h.client.resolveCalls)
	assert.Equal(t, 1, h.client.listCommentCalls)
	assert.Zero(t, h.client.publishCalls)
	assert.Zero(t, h.client.completeCalls)

	mapping, err := h.store.ImportMappingBySource(t.Context(), h.project.ID, "connector:notes", "issue", "root-1")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	assert.Equal(t, h.issue.ID, *mapping.IssueID)
}

func TestBindUsesExistingCommentRevisionsAsInboundFrontier(t *testing.T) {
	h := newServiceHarness(t)
	h.client.resolved.UpdatedAt = testObservedAt
	h.client.read.UpdatedAt = testObservedAt
	providerCommentAt := testObservedAt.Add(time.Minute)
	h.client.comments = []connector.Comment{{
		ID: "existing-comment", Revision: "existing-comment-revision-1", Body: "Existing provider comment",
		Author:    connector.Actor{ID: "reviewer", DisplayName: "Reviewer"},
		CreatedAt: providerCommentAt, UpdatedAt: providerCommentAt,
	}}

	binding, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	reconciler := NewReconciler(h.observed, h.registry, ReconcilerConfig{
		Now: func() time.Time { return testObservedAt.Add(2 * time.Minute) },
	})

	initial, err := reconciler.Run(t.Context(), binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, initial.CommentsCreated)
	comments, err := h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Empty(t, comments)

	h.client.comments[0].Revision = "existing-comment-revision-2"
	h.client.comments[0].Body = "Provider comment edited after binding"
	edited, err := reconciler.Run(t.Context(), binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, edited.CommentsCreated)
	comments, err = h.store.CommentsByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "Provider comment edited after binding", comments[0].Body)
}

func TestBindPublishingMarksExistingCommentsAsSkipped(t *testing.T) {
	h := newServiceHarness(t)
	oldObservation := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	h.client.resolved.ObservedAt = oldObservation
	comment, _, err := h.store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: h.issue.ID, Author: "tester", Body: "Existing local comment",
	})
	require.NoError(t, err)
	storedCommentAt := time.Date(2036, 4, 5, 6, 7, 8, 321_000_000, time.UTC)
	_, err = h.store.ExecContext(t.Context(),
		`UPDATE comments SET created_at=? WHERE id=?`, storedCommentAt.Format(time.RFC3339Nano), comment.ID)
	require.NoError(t, err)
	comment.CreatedAt = storedCommentAt

	binding, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester", PublishComments: true,
	})
	require.NoError(t, err)
	require.NotNil(t, binding.ReceiveCommentsAfter)
	require.NotNil(t, binding.PublishCommentsAfter)
	assert.Equal(t, oldObservation, *binding.ReceiveCommentsAfter)
	assert.True(t, binding.PublishCommentsAfter.IsZero())
	mapping, err := h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootSkippedCommentMappingSource("notes"), "comment", comment.UID,
	)
	require.NoError(t, err)
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, comment.ID, *mapping.CommentID)
}

func TestBindPublishingRequiresConnectorCapabilityBeforeMutation(t *testing.T) {
	h := newServiceHarness(t)
	h.client.description.Capabilities = nil

	_, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester", PublishComments: true,
	})
	assert.ErrorIs(t, err, ErrCommentPublishingUnavailable)
	_, bindingErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	_, mappingErr := h.store.ImportMappingBySource(
		t.Context(), h.project.ID, "connector:notes", "issue", "root-1",
	)
	assert.ErrorIs(t, mappingErr, db.ErrNotFound)
	assert.Zero(t, h.client.publishCalls)
}

func TestBindPublishingRequiresSelfActorBeforeMutation(t *testing.T) {
	h := newServiceHarness(t)
	h.client.description.SelfActorID = " \t "

	_, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester", PublishComments: true,
	})
	assert.ErrorIs(t, err, ErrCommentPublishingUnavailable)
	_, bindingErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	_, mappingErr := h.store.ImportMappingBySource(
		t.Context(), h.project.ID, "connector:notes", "issue", "root-1",
	)
	assert.ErrorIs(t, mappingErr, db.ErrNotFound)
	assert.Zero(t, h.client.publishCalls)
}

func TestBindWaitsForImmediateReconcileAndReadsIssueAndBindingAfterward(t *testing.T) {
	h := newServiceHarness(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	h.observed.hookFinished = false
	h.service.immediateClaimedReconcile = func(ctx context.Context, bindingID int64, claimToken string) ([]db.Event, error) {
		close(entered)
		<-release
		_, event, _, err := h.store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: bindingID, ClaimToken: claimToken,
			Title: h.client.resolved.Title, Body: h.client.resolved.Body,
			ExternalRevision:  h.client.resolved.Revision,
			ExternalUpdatedAt: testObservedAt, ExternalObservedAt: testObservedAt,
			IntegrationActor: "connector:notes",
		})
		h.observed.hookFinished = true
		if err != nil || event == nil {
			return nil, err
		}
		return []db.Event{*event}, nil
	}

	type result struct {
		binding db.ExternalRootBinding
		events  []db.Event
		err     error
	}
	done := make(chan result, 1)
	go func() {
		binding, events, err := h.service.Bind(context.Background(), BindParams{
			ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
			Locator: "root-1", Actor: "tester",
		})
		done <- result{binding: binding, events: events, err: err}
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("bind returned before immediate reconciliation completed")
	default:
	}
	close(release)
	got := <-done
	require.NoError(t, got.err)
	assert.Len(t, got.events, 2)
	assert.True(t, h.observed.issueReadAfterHook)
	assert.True(t, h.observed.bindingReadAfterHook)
	issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "External plan", issue.Title)
	assert.Equal(t, "External body", issue.Body)
}

func TestBindDeliversCommittedBindingBeforeImmediateReconcileReturns(t *testing.T) {
	h := newServiceHarness(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan db.Event, 2)
	h.service = NewServiceWithEventSink(
		h.observed,
		h.registry,
		func(context.Context, int64, string) ([]db.Event, error) {
			close(entered)
			<-release
			return nil, nil
		},
		func(event db.Event) { delivered <- event },
	)
	type outcome struct {
		events []db.Event
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		_, events, err := h.service.Bind(context.Background(), BindParams{
			ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
			Locator: "root-1", Actor: "tester",
		})
		done <- outcome{events: events, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		require.FailNow(t, "immediate reconciliation did not start")
	}
	var eventBeforeRelease db.Event
	select {
	case eventBeforeRelease = <-delivered:
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	got := <-done
	require.NoError(t, got.err)
	require.NotZero(t, eventBeforeRelease.ID, "binding event was buffered behind immediate reconciliation")
	assert.Equal(t, "issue.external_root_bound", eventBeforeRelease.Type)
	assert.Len(t, got.events, 1)
}

func TestBindCommitsReservedClaimBeforeImmediateReconcile(t *testing.T) {
	h := newServiceHarness(t)
	var receivedToken string
	h.service.immediateClaimedReconcile = func(
		ctx context.Context,
		bindingID int64,
		claimToken string,
	) ([]db.Event, error) {
		stored, err := h.store.ExternalRootBindingByID(ctx, bindingID)
		if err != nil {
			return nil, err
		}
		if stored.ClaimToken != claimToken || stored.ClaimStartedAt == nil {
			return nil, errors.New("reserved claim was not committed before reconciliation")
		}
		receivedToken = claimToken
		_, acquired, err := h.store.ClaimExternalRootBinding(
			ctx,
			bindingID,
			"competing-claim",
			stored.ClaimStartedAt.Add(time.Second),
			stored.ClaimStartedAt.Add(-time.Minute),
		)
		if err != nil {
			return nil, err
		}
		if acquired {
			return nil, errors.New("competing reconcile acquired the reserved binding")
		}
		return nil, nil
	}

	binding, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	assert.True(t, katauid.Valid(receivedToken))
	assert.Equal(t, receivedToken, binding.ClaimToken)
	require.NotNil(t, binding.ClaimStartedAt)
}

func TestBindCompleteRootDelegatesLaterReviewWithoutClosingKata(t *testing.T) {
	h := newServiceHarness(t)
	h.client.resolved.State = "complete"
	h.service.immediateClaimedReconcile = func(ctx context.Context, _ int64, _ string) ([]db.Event, error) {
		_, labelEvent, err := h.store.AddLabelAndEvent(ctx, h.issue.ID, db.LabelEventParams{
			EventType: "issue.labeled", Label: "needs-review", Actor: "connector:notes",
		})
		if err != nil {
			return nil, err
		}
		_, commentEvent, err := h.store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: h.issue.ID, Author: "connector:notes", Body: "Review external completion.",
		})
		return []db.Event{labelEvent, commentEvent}, err
	}

	_, events, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	assert.Len(t, events, 3)
	issue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
	hasReview, err := h.store.HasLabel(t.Context(), h.issue.ID, "needs-review")
	require.NoError(t, err)
	assert.True(t, hasReview)
	assert.Zero(t, h.client.completeCalls)
}

func TestBindDoesNotPropagateToChildren(t *testing.T) {
	h := newServiceHarness(t)
	child, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.project.ID, Title: "Child", Author: "tester",
	})
	require.NoError(t, err)
	_, err = h.store.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: h.issue.ID, Type: "parent", Author: "tester",
	})
	require.NoError(t, err)

	_, _, err = h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	_, err = h.store.ExternalRootBindingByIssue(t.Context(), child.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestBindRejectsIdentityMismatchWithoutChangingState(t *testing.T) {
	h := newServiceHarness(t)
	h.client.resolved.IdentityKey = "account-2"

	_, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	assert.ErrorIs(t, err, ErrConnectorIdentityChanged)
	_, err = h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestBindRejectsNoncanonicalResolvedIdentityWithoutChangingState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*connector.Root)
	}{
		{name: "root key", mutate: func(root *connector.Root) { root.Key = " " + root.Key }},
		{name: "account identity", mutate: func(root *connector.Root) { root.IdentityKey += " " }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			test.mutate(&h.client.resolved)

			_, _, err := h.service.Bind(t.Context(), BindParams{
				ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
				Locator: "root-1", Actor: "tester",
			})

			assert.ErrorIs(t, err, db.ErrExternalRootValidation)
			_, err = h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
			assert.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

func TestBindRejectsUnreconcilableConnectorDataWithoutChangingState(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*fakeConnectorClient)
		wantError error
	}{
		{
			name: "blank root title",
			mutate: func(client *fakeConnectorClient) {
				client.resolved.Title = " \t "
			},
			wantError: db.ErrExternalRootValidation,
		},
		{
			name: "blank live comment body",
			mutate: func(client *fakeConnectorClient) {
				client.comments = []connector.Comment{{
					ID: "comment-1", Revision: "revision-1", Body: " \t ",
					Author:    connector.Actor{ID: "reviewer", DisplayName: "Reviewer"},
					CreatedAt: testObservedAt, UpdatedAt: testObservedAt,
				}}
			},
			wantError: connectorclient.ErrProtocolFailure,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			test.mutate(h.client)
			before, err := h.store.MaxEventID(t.Context())
			require.NoError(t, err)

			_, _, err = h.service.Bind(t.Context(), BindParams{
				ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
				Locator: "root-1", Actor: "tester",
			})

			assert.ErrorIs(t, err, test.wantError)
			_, bindingErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
			assert.ErrorIs(t, bindingErr, db.ErrNotFound)
			after, eventErr := h.store.MaxEventID(t.Context())
			require.NoError(t, eventErr)
			assert.Equal(t, before, after)
		})
	}
}

func TestBindMissingInstanceDoesNotChangeState(t *testing.T) {
	h := newServiceHarness(t)
	_, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "missing",
		Locator: "root-1", Actor: "tester",
	})
	assert.ErrorIs(t, err, ErrConnectorInstanceNotFound)
	_, err = h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestBindLeavesParentCancellationOutsideConnectorFailure(t *testing.T) {
	h := newServiceHarness(t)
	h.client.resolveErr = errors.New("opaque connector cancellation detail")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := h.service.Bind(ctx, BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})

	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrConnectorCall)
	assert.NotContains(t, err.Error(), "opaque connector cancellation detail")
	_, bindingErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
}

func TestBindParentCancellationDuringDescribeDoesNotMutateRegistryHealth(t *testing.T) {
	h := newServiceHarness(t)
	h.registry.recordDescribeHealthError("notes", ErrConnectorCall)
	wantHealth, ok := h.registry.Snapshot("notes")
	require.True(t, ok)
	require.NotEmpty(t, wantHealth.HealthError)
	ctx, cancel := context.WithCancel(t.Context())
	h.client.beforeDescribeReturn = cancel
	h.client.describeErr = errors.New("opaque connector cancellation detail")

	_, _, err := h.service.Bind(ctx, BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})

	require.ErrorIs(t, err, context.Canceled)
	snapshot, ok := h.registry.Snapshot("notes")
	require.True(t, ok)
	assert.Equal(t, wantHealth.HealthError, snapshot.HealthError)
	_, bindingErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
}

func TestBindConnectorFailureRemainsHealthWhenParentExpiresAfterBoundary(t *testing.T) {
	h := newServiceHarness(t)
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

	_, _, err := h.service.Bind(ctx, BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})

	require.ErrorIs(t, err, ErrConnectorCall)
	require.ErrorIs(t, err, connectorclient.ErrRequestTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	snapshot, ok := h.registry.Snapshot("notes")
	require.True(t, ok)
	assert.Equal(t, "external connector request timed out", snapshot.HealthError)
	_, bindingErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
}

func TestResumeVerifiesDescriptorAccountAndRootBeforeEnabling(t *testing.T) {
	h := newServiceHarness(t)
	_, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	_, _, err = h.service.Pause(t.Context(), h.issue.ID, "tester", "operator_pause")
	require.NoError(t, err)
	h.client.description.AccountIdentity = "account-2"

	_, _, err = h.service.Resume(t.Context(), h.issue.ID, "tester")
	assert.ErrorIs(t, err, ErrConnectorIdentityChanged)
	got, err := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
	require.NoError(t, err)
	assert.False(t, got.Enabled)
}

func TestResumePublishingRequiresCurrentCapabilityAndSelfActor(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*connector.Description)
	}{
		{
			name: "publish capability removed",
			mutate: func(description *connector.Description) {
				description.Capabilities = nil
			},
		},
		{
			name: "self actor removed",
			mutate: func(description *connector.Description) {
				description.SelfActorID = " \t "
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			_, _, err := h.service.Bind(t.Context(), BindParams{
				ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
				Locator: "root-1", Actor: "tester", PublishComments: true,
			})
			require.NoError(t, err)
			paused, _, err := h.service.Pause(t.Context(), h.issue.ID, "tester", "operator_pause")
			require.NoError(t, err)
			require.False(t, paused.Enabled)
			cursor, err := h.store.MaxEventID(t.Context())
			require.NoError(t, err)

			test.mutate(&h.client.description)
			_, _, err = h.service.Resume(t.Context(), h.issue.ID, "tester")
			assert.ErrorIs(t, err, ErrCommentPublishingUnavailable)
			assert.Zero(t, h.client.readCalls)
			stored, readErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.False(t, stored.Enabled)
			assert.Equal(t, "operator_pause", stored.PausedReason)
			events, eventsErr := h.store.EventsAfter(t.Context(), db.EventsAfterParams{
				AfterID: cursor,
				Limit:   10,
			})
			require.NoError(t, eventsErr)
			assert.Empty(t, events)
		})
	}
}

func TestResumeVerifiesConnectorIDProtocolAndRootKey(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeConnectorClient)
	}{
		{name: "connector id", mutate: func(c *fakeConnectorClient) { c.description.ConnectorID = "changed.connector" }},
		{name: "protocol", mutate: func(c *fakeConnectorClient) { c.description.Protocol = "changed.protocol" }},
		{name: "root key", mutate: func(c *fakeConnectorClient) { c.read.Key = "root-2" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			_, _, err := h.service.Bind(t.Context(), BindParams{
				ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
				Locator: "root-1", Actor: "tester",
			})
			require.NoError(t, err)
			_, _, err = h.service.Pause(t.Context(), h.issue.ID, "tester", "operator_pause")
			require.NoError(t, err)
			test.mutate(h.client)
			_, _, err = h.service.Resume(t.Context(), h.issue.ID, "tester")
			assert.ErrorIs(t, err, ErrConnectorIdentityChanged)
			binding, getErr := h.store.ExternalRootBindingByIssue(t.Context(), h.issue.ID)
			require.NoError(t, getErr)
			assert.False(t, binding.Enabled)
		})
	}
}

func TestPauseResumeAndUnbindPreserveStorageEvents(t *testing.T) {
	h := newServiceHarness(t)
	_, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	paused, event, err := h.service.Pause(t.Context(), h.issue.ID, "tester", "operator_pause")
	require.NoError(t, err)
	assert.False(t, paused.Enabled)
	assert.Equal(t, "issue.external_root_paused", event.Type)
	resumed, event, err := h.service.Resume(t.Context(), h.issue.ID, "tester")
	require.NoError(t, err)
	assert.True(t, resumed.Enabled)
	assert.Equal(t, "issue.external_root_resumed", event.Type)
	unbound, event, err := h.service.Unbind(t.Context(), h.issue.ID, "tester")
	require.NoError(t, err)
	assert.False(t, unbound.Active)
	assert.Equal(t, "issue.external_root_unbound", event.Type)
}

func TestPauseAndUnbindRejectLiveWorkerClaim(t *testing.T) {
	h := newServiceHarness(t)
	binding, _, err := h.service.Bind(t.Context(), BindParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID, ConnectorInstance: "notes",
		Locator: "root-1", Actor: "tester",
	})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	claimed, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), binding.ID, "active-worker", now, now.Add(-h.service.claimStaleAfter),
	)
	require.NoError(t, err)
	require.True(t, acquired)

	_, _, err = h.service.Pause(t.Context(), h.issue.ID, "tester", "operator_pause")
	assert.ErrorIs(t, err, db.ErrExternalRootClaimActive)
	_, _, err = h.service.Unbind(t.Context(), h.issue.ID, "tester")
	assert.ErrorIs(t, err, db.ErrExternalRootClaimActive)
	retained, readErr := h.store.ExternalRootBindingByID(t.Context(), binding.ID)
	require.NoError(t, readErr)
	assert.True(t, retained.Active)
	assert.True(t, retained.Enabled)
	assert.Equal(t, claimed.ClaimToken, retained.ClaimToken)
}

func TestMappingUsesStableFieldIDAndRefreshesDescriptor(t *testing.T) {
	h := newServiceHarness(t)
	h.client.fields = []connector.FieldDescriptor{
		{ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "1"},
		{ID: "field-2", DisplayName: "field-1", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "1"},
	}
	first, err := h.service.MapField(t.Context(), MapFieldParams{
		ConnectorInstance: "notes", KataField: "scheduled_on", ExternalField: "field-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "field-1", first.ExternalFieldID)
	assert.Equal(t, "Start", first.ExternalFieldName)

	h.client.fields[0].DisplayName = "Start date"
	h.client.fields[0].SchemaRevision = "2"
	second, err := h.service.MapField(t.Context(), MapFieldParams{
		ConnectorInstance: "notes", KataField: "scheduled_on", ExternalField: "field-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "field-1", second.ExternalFieldID)
	assert.Equal(t, "Start date", second.ExternalFieldName)
	assert.Equal(t, "2", second.SchemaRevision)
	assert.NotEqual(t, first.ID, second.ID)
	mappings, err := h.store.ListExternalFieldMappings(t.Context(), "notes")
	require.NoError(t, err)
	require.Len(t, mappings, 2)
	assert.False(t, mappings[0].Active)
	assert.True(t, mappings[1].Active)
}

func TestMappingRequiresUniqueDisplayNameAndValidCodecDescriptor(t *testing.T) {
	h := newServiceHarness(t)
	h.client.fields = []connector.FieldDescriptor{
		{ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "1"},
		{ID: "field-2", DisplayName: "Start", AcceptedKinds: []string{"instant"}, Nullable: true, Writable: true, SchemaRevision: "1"},
	}
	_, err := h.service.MapField(t.Context(), MapFieldParams{ConnectorInstance: "notes", KataField: "scheduled_on", ExternalField: "Start"})
	assert.ErrorIs(t, err, ErrExternalFieldAmbiguous)

	h.client.fields = []connector.FieldDescriptor{{
		ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{"text"}, Nullable: true, Writable: true, SchemaRevision: "1",
	}}
	_, err = h.service.MapField(t.Context(), MapFieldParams{ConnectorInstance: "notes", KataField: "scheduled_on", ExternalField: "field-1"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unsupported external field kind")
}

func TestMappingRejectsNonCanonicalFieldDescriptorIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		descriptor connector.FieldDescriptor
		protocol   bool
	}{
		{
			name: "padded field ID",
			descriptor: connector.FieldDescriptor{
				ID: " field-1 ", DisplayName: "Start", AcceptedKinds: []string{"date"},
				Nullable: true, Writable: true, SchemaRevision: "schema-1",
			},
			protocol: true,
		},
		{
			name: "padded schema revision",
			descriptor: connector.FieldDescriptor{
				ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{"date"},
				Nullable: true, Writable: true, SchemaRevision: " schema-1 ",
			},
			protocol: true,
		},
		{
			name: "padded accepted kind",
			descriptor: connector.FieldDescriptor{
				ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{" date "},
				Nullable: true, Writable: true, SchemaRevision: "schema-1",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			h.client.fields = []connector.FieldDescriptor{test.descriptor}

			_, err := h.service.MapField(t.Context(), MapFieldParams{
				ConnectorInstance: "notes", KataField: "scheduled_on", ExternalField: "Start",
			})
			if test.protocol {
				require.ErrorIs(t, err, connectorclient.ErrProtocolFailure)
			} else {
				require.Error(t, err)
			}
			mappings, listErr := h.store.ListExternalFieldMappings(t.Context(), "notes")
			require.NoError(t, listErr)
			assert.Empty(t, mappings)
		})
	}
}

func TestMappingRequiresFieldCapabilitiesBeforeDiscovery(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities []connector.Capability
	}{
		{name: "fields unavailable", capabilities: []connector.Capability{connector.CapabilityConditionalFields}},
		{name: "conditional writes unavailable", capabilities: []connector.Capability{connector.CapabilityFields}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			h.client.description.Capabilities = test.capabilities
			h.client.fields = []connector.FieldDescriptor{{
				ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{"date"},
				Nullable: true, Writable: true, SchemaRevision: "1",
			}}

			_, err := h.service.MapField(t.Context(), MapFieldParams{
				ConnectorInstance: "notes", KataField: "scheduled_on", ExternalField: "field-1",
			})

			assert.ErrorIs(t, err, ErrFieldSynchronizationUnavailable)
			assert.Zero(t, h.client.listFieldsCalls)
		})
	}
}

func TestUnmapFieldDeactivatesMappingAndRetainsHistory(t *testing.T) {
	h := newServiceHarness(t)
	h.client.fields = []connector.FieldDescriptor{{
		ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "1",
	}}
	mapped, err := h.service.MapField(t.Context(), MapFieldParams{ConnectorInstance: "notes", KataField: "scheduled_on", ExternalField: "field-1"})
	require.NoError(t, err)
	unmapped, err := h.service.UnmapField(t.Context(), "notes", "scheduled_on")
	require.NoError(t, err)
	assert.Equal(t, mapped.ID, unmapped.ID)
	assert.False(t, unmapped.Active)
	mappings, err := h.store.ListExternalFieldMappings(t.Context(), "notes")
	require.NoError(t, err)
	require.Len(t, mappings, 1)
	assert.False(t, mappings[0].Active)
}

func TestResolveFieldConflictWritesOnlyTheSelectedSideAndClearsCandidates(t *testing.T) {
	for _, use := range []string{"kata", "external"} {
		t.Run(use, func(t *testing.T) {
			h, service := prepareFieldConflict(t)
			beforeWrites := len(h.client.writeFieldCalls)

			state, events, err := service.ResolveFieldConflict(
				t.Context(), h.issue.ID, "scheduled_on", use, "operator",
			)
			require.NoError(t, err)
			assert.False(t, state.Conflicted)
			assert.Empty(t, state.ConflictKata)
			assert.Empty(t, state.ConflictExternal)
			want := date("2026-08-21")
			if use == "external" {
				want = date("2026-08-22")
			}
			assert.Equal(t, want, decodeStoredFieldValue(t, state.Baseline))
			if use == "kata" {
				assert.Equal(t, beforeWrites+1, len(h.client.writeFieldCalls))
			} else {
				assert.Equal(t, beforeWrites, len(h.client.writeFieldCalls))
			}
			assert.Equal(t, want, h.client.fieldValues["start-date"])
			issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			kataValue, readErr := fieldCodecs["scheduled_on"].ReadKata(issue)
			require.NoError(t, readErr)
			assert.Equal(t, want, kataValue)
			resolved := requireEventType(t, events, "issue.external_field_resolved")
			assert.Equal(t, "operator", resolved.Actor)
			assert.NotContains(t, resolved.Payload, "2026-08-21")
			assert.NotContains(t, resolved.Payload, "2026-08-22")
			assert.NotContains(t, resolved.Payload, h.binding.ExternalAccountKey)
			binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
			require.NoError(t, readErr)
			assert.Empty(t, binding.ClaimToken)
		})
	}
}

type failFieldConflictResolutionOnceStorage struct {
	db.Storage
	err    error
	failed bool
}

func (s *failFieldConflictResolutionOnceStorage) ResolveExternalFieldConflict(
	ctx context.Context,
	params db.ResolveExternalFieldConflictParams,
) (db.ExternalFieldState, db.Event, error) {
	if !s.failed {
		s.failed = true
		return db.ExternalFieldState{}, db.Event{}, s.err
	}
	return s.Storage.ResolveExternalFieldConflict(ctx, params)
}

func TestResolveFieldConflictResumesAfterSelectedSideWasAlreadyApplied(t *testing.T) {
	for _, use := range []string{"kata", "external"} {
		t.Run(use, func(t *testing.T) {
			h, service := prepareFieldConflict(t)
			postWriteErr := errors.New("synthetic conflict finalization failure")
			store := &failFieldConflictResolutionOnceStorage{Storage: h.store, err: postWriteErr}
			service.store = store

			_, _, err := service.ResolveFieldConflict(
				t.Context(), h.issue.ID, "scheduled_on", use, "operator",
			)
			require.ErrorIs(t, err, postWriteErr)
			issueAfterWrite, err := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, err)
			writesAfterFirst := len(h.client.writeFieldCalls)

			state, events, err := service.ResolveFieldConflict(
				t.Context(), h.issue.ID, "scheduled_on", use, "operator",
			)
			require.NoError(t, err)
			assert.False(t, state.Conflicted)
			assert.Contains(t, eventTypes(events), "issue.external_field_resolved")
			issueAfterRetry, err := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, err)
			assert.Equal(t, issueAfterWrite.Revision, issueAfterRetry.Revision)
			assert.Len(t, h.client.writeFieldCalls, writesAfterFirst)
		})
	}
}

func TestResolveFieldConflictAllowsCoordinatedLocalTimezoneTransition(t *testing.T) {
	h := newReconcileHarness(t)
	startMapping := h.mapField(t, "scheduled_on", "start-date")
	dueMapping := h.mapField(t, "deadline_on", "due-date")
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
	claimToken := "seed-coordinated-conflicts"
	_, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, claimToken, h.now, h.now.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	for _, pair := range []struct {
		mapping  db.ExternalFieldMapping
		baseline connector.FieldValue
		external connector.FieldValue
	}{
		{mapping: startMapping, baseline: oldStart, external: newStart},
		{mapping: dueMapping, baseline: oldDue, external: newDue},
	} {
		baseline, marshalErr := json.Marshal(pair.baseline)
		require.NoError(t, marshalErr)
		external, marshalErr := json.Marshal(pair.external)
		require.NoError(t, marshalErr)
		_, _, err = h.store.UpsertExternalFieldState(t.Context(), db.ExternalFieldStateParams{
			BindingID: h.binding.ID, MappingID: pair.mapping.ID, ClaimToken: claimToken,
			Baseline: baseline, ConflictKata: baseline, ConflictExternal: external,
			Conflicted: true, At: h.now, Actor: integrationActor(h.binding),
		})
		require.NoError(t, err)
	}
	_, err = h.store.ReleaseExternalRootClaim(t.Context(), h.binding.ID, claimToken)
	require.NoError(t, err)
	service := NewService(h.store, h.registry, nil)

	state, events, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "scheduled_on", "external", "operator",
	)

	require.NoError(t, err)
	assert.False(t, state.Conflicted)
	assert.Equal(t, newStart, decodeStoredFieldValue(t, state.Baseline))
	assert.Contains(t, eventTypes(events), "issue.external_field_resolved")
	assert.True(t, h.fieldState(t, "deadline_on").Conflicted)
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	values, readErr := metadataObject(issue.Metadata)
	require.NoError(t, readErr)
	assert.JSONEq(t, `"2026-08-22T09:30"`, string(values["scheduled_on"]))
	assert.JSONEq(t, `"America/Los_Angeles"`, string(values["timezone"]))

	dueState, dueEvents, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "deadline_on", "external", "operator",
	)
	require.NoError(t, err)
	assert.False(t, dueState.Conflicted)
	assert.Equal(t, newDue, decodeStoredFieldValue(t, dueState.Baseline))
	assert.Contains(t, eventTypes(dueEvents), "issue.external_field_resolved")
	issue, readErr = h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	values, readErr = metadataObject(issue.Metadata)
	require.NoError(t, readErr)
	assert.JSONEq(t, `"2026-08-22T10:30"`, string(values["deadline_on"]))
	assert.JSONEq(t, `"America/Los_Angeles"`, string(values["timezone"]))
}

func TestResolveFieldConflictRequiresCurrentFieldsCapability(t *testing.T) {
	h, service := prepareFieldConflict(t)
	h.client.description.Capabilities = []connector.Capability{connector.CapabilityPublishComment}
	listCalls := h.client.listFieldsCalls
	readCalls := len(h.client.readFieldCalls)
	writeCalls := len(h.client.writeFieldCalls)

	_, _, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "scheduled_on", "kata", "operator",
	)

	assert.ErrorIs(t, err, ErrFieldSynchronizationUnavailable)
	assert.Equal(t, listCalls, h.client.listFieldsCalls)
	assert.Len(t, h.client.readFieldCalls, readCalls)
	assert.Len(t, h.client.writeFieldCalls, writeCalls)
	retained := h.fieldState(t, "scheduled_on")
	assert.True(t, retained.Conflicted)
	assert.Empty(t, h.requireBinding(t).ClaimToken)
}

func TestResolveFieldConflictBypassesFutureNextAttempt(t *testing.T) {
	h, service := prepareFieldConflict(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	nextAttemptAt := at.Add(time.Hour)
	claimed, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, "failed-attempt", at, at.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = h.store.RecordExternalRootError(t.Context(), db.ExternalRootErrorParams{
		BindingID: claimed.ID, ClaimToken: claimed.ClaimToken,
		At: at, NextAttemptAt: nextAttemptAt, Error: "temporary failure",
	})
	require.NoError(t, err)
	backedOff, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	require.NotNil(t, backedOff.NextAttemptAt)
	assert.Equal(t, nextAttemptAt, *backedOff.NextAttemptAt)
	before := h.fieldState(t, "scheduled_on")
	require.True(t, before.Conflicted)

	state, events, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "scheduled_on", "kata", "operator",
	)

	require.NoError(t, err)
	assert.False(t, state.Conflicted)
	assert.Equal(t, date("2026-08-21"), decodeStoredFieldValue(t, state.Baseline))
	assert.Contains(t, eventTypes(events), "issue.external_field_resolved")
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
	require.NotNil(t, binding.NextAttemptAt)
	assert.Equal(t, nextAttemptAt, *binding.NextAttemptAt)
}

func TestResolveFieldConflictRenewsClaimDuringLongConnectorWrite(t *testing.T) {
	h, service := prepareFieldConflict(t)
	const staleAfter = 120 * time.Millisecond
	service.claimStaleAfter = staleAfter
	entered := make(chan struct{})
	release := make(chan struct{})
	h.client.beforeWriteFields = func() {
		close(entered)
		<-release
	}
	type outcome struct {
		state  db.ExternalFieldState
		events []db.Event
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		state, events, err := service.ResolveFieldConflict(
			context.Background(), h.issue.ID, "scheduled_on", "kata", "operator",
		)
		done <- outcome{state: state, events: events, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "field write did not start")
	}
	time.Sleep(3 * staleAfter)
	now := time.Now().UTC()
	_, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, "competing-field-resolution", now, now.Add(-staleAfter),
	)
	require.NoError(t, err)
	close(release)
	completed := <-done

	assert.False(t, acquired, "a valid manual field resolution must retain its exclusive claim")
	require.NoError(t, completed.err)
	assert.False(t, completed.state.Conflicted)
	assert.Contains(t, eventTypes(completed.events), "issue.external_field_resolved")
}

func TestResolveFieldConflictRejectsProviderDriftDuringWrite(t *testing.T) {
	h, service := prepareFieldConflict(t)
	h.client.beforeWriteFields = func() {
		h.client.fieldValues["start-date"] = date("2026-08-24")
	}

	_, events, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "scheduled_on", "kata", "operator",
	)

	var connectorErr *connector.Error
	require.ErrorAs(t, err, &connectorErr)
	assert.Equal(t, "field_conflict", connectorErr.Code)
	assert.Empty(t, events)
	assert.Equal(t, date("2026-08-24"), h.client.fieldValues["start-date"])
	state := h.fieldState(t, "scheduled_on")
	assert.True(t, state.Conflicted)
	assert.Equal(t, date("2026-08-21"), decodeStoredFieldValue(t, state.ConflictKata))
	assert.Equal(t, date("2026-08-22"), decodeStoredFieldValue(t, state.ConflictExternal))
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
}

func TestResolveFieldConflictRejectsDriftWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *reconcileHarness)
	}{
		{name: "Kata candidate changed", mutate: func(t *testing.T, h *reconcileHarness) {
			h.setKataField(t, "scheduled_on", date("2026-08-23"))
		}},
		{name: "external candidate changed", mutate: func(_ *testing.T, h *reconcileHarness) {
			h.client.fieldValues["start-date"] = date("2026-08-24")
		}},
		{name: "external candidate invalid", mutate: func(_ *testing.T, h *reconcileHarness) {
			h.client.fieldValues["start-date"] = connector.FieldValue{Kind: "text", Value: "invalid-value"}
		}},
		{name: "descriptor changed", mutate: func(_ *testing.T, h *reconcileHarness) {
			h.client.fields[0].SchemaRevision = "schema-2"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, service := prepareFieldConflict(t)
			test.mutate(t, h)
			beforeIssue, err := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, err)
			beforeWrites := len(h.client.writeFieldCalls)

			_, events, err := service.ResolveFieldConflict(
				t.Context(), h.issue.ID, "scheduled_on", "kata", "operator",
			)
			assert.ErrorIs(t, err, ErrFieldConflictChanged)
			assert.Empty(t, events)
			assert.Equal(t, beforeWrites, len(h.client.writeFieldCalls))
			afterIssue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, readErr)
			assert.Equal(t, beforeIssue.Revision, afterIssue.Revision)
			state := h.fieldState(t, "scheduled_on")
			assert.True(t, state.Conflicted)
			assert.Equal(t, date("2026-08-21"), decodeStoredFieldValue(t, state.ConflictKata))
			assert.Equal(t, date("2026-08-22"), decodeStoredFieldValue(t, state.ConflictExternal))
			binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
			require.NoError(t, readErr)
			assert.Empty(t, binding.ClaimToken)
		})
	}
}

func TestResolveFieldConflictRechecksStoredCandidatesAfterClaim(t *testing.T) {
	h, service := prepareFieldConflict(t)
	state := h.fieldState(t, "scheduled_on")
	refreshedKata, err := marshalCanonicalFieldValue(date("2026-08-23"))
	require.NoError(t, err)
	refreshedExternal, err := marshalCanonicalFieldValue(date("2026-08-24"))
	require.NoError(t, err)
	service.store = &refreshConflictAfterClaimStorage{
		Storage: h.store, mappingID: state.MappingID, baseline: state.Baseline,
		kata: refreshedKata, external: refreshedExternal,
	}
	beforeIssue, err := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, err)
	beforeWrites := len(h.client.writeFieldCalls)

	_, events, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "scheduled_on", "kata", "operator",
	)

	assert.ErrorIs(t, err, ErrFieldConflictChanged)
	assert.Empty(t, events)
	assert.Equal(t, beforeWrites, len(h.client.writeFieldCalls))
	afterIssue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	assert.Equal(t, beforeIssue.Revision, afterIssue.Revision)
	refreshed := h.fieldState(t, "scheduled_on")
	assert.True(t, refreshed.Conflicted)
	assert.Equal(t, date("2026-08-23"), decodeStoredFieldValue(t, refreshed.ConflictKata))
	assert.Equal(t, date("2026-08-24"), decodeStoredFieldValue(t, refreshed.ConflictExternal))
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
}

func TestResolveFieldConflictRequiresExplicitCandidateAndConflict(t *testing.T) {
	h, service := prepareFieldConflict(t)
	_, _, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "scheduled_on", "newest", "operator",
	)
	assert.ErrorIs(t, err, db.ErrExternalRootValidation)

	h.mapField(t, "deadline_on", "due-date")
	_, _, err = service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "deadline_on", "kata", "operator",
	)
	assert.ErrorIs(t, err, db.ErrExternalFieldNotConflicted)
}

func TestResolveFieldConflictFinalizesClaimAfterCancellationFollowingCommit(t *testing.T) {
	h, service := prepareFieldConflict(t)
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelAfterFieldResolutionStorage{Storage: h.store, cancel: cancel}
	service.store = store

	state, events, err := service.ResolveFieldConflict(
		ctx, h.issue.ID, "scheduled_on", "kata", "operator",
	)

	require.NoError(t, err)
	assert.False(t, state.Conflicted)
	assert.Contains(t, eventTypes(events), "issue.external_field_resolved")
	assert.NoError(t, store.releaseCtxErr)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
}

func TestResolveFieldConflictUsingExternalValueDoesNotDependOnProviderWrite(t *testing.T) {
	h, service := prepareFieldConflict(t)
	connectorErr := &connector.Error{Code: "temporarily_unavailable", Message: "field write unavailable"}
	h.client.writeFieldsErr = connectorErr
	beforeWrites := len(h.client.writeFieldCalls)
	beforeReads := len(h.client.readFieldCalls)

	resolved, events, err := service.ResolveFieldConflict(
		t.Context(), h.issue.ID, "scheduled_on", "external", "operator",
	)

	require.NoError(t, err)
	assert.False(t, resolved.Conflicted)
	assert.Equal(t, beforeWrites, len(h.client.writeFieldCalls))
	assert.Equal(t, beforeReads+1, len(h.client.readFieldCalls))
	assert.Contains(t, eventTypes(events), "issue.metadata_updated")
	assert.Contains(t, eventTypes(events), "issue.external_field_resolved")
	state := h.fieldState(t, "scheduled_on")
	assert.False(t, state.Conflicted)
	assert.Equal(t, date("2026-08-22"), decodeStoredFieldValue(t, state.Baseline))
	issue, readErr := h.store.IssueByID(t.Context(), h.issue.ID)
	require.NoError(t, readErr)
	value, readErr := fieldCodecs["scheduled_on"].ReadKata(issue)
	require.NoError(t, readErr)
	assert.Equal(t, date("2026-08-22"), value)
	binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, binding.ClaimToken)
}

func TestResolvePendingCommentAllowsPausedBindingUnderExclusiveActionClaim(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	local := h.createLocalComment(t, "Adopt after pause")
	claimed, ok, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, "setup-claim", h.now, h.now.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = h.store.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{
		BindingID: claimed.ID, ClaimToken: claimed.ClaimToken, CommentUID: local.UID, At: h.now,
	})
	require.NoError(t, err)
	_, _, err = h.store.PauseExternalRootBinding(t.Context(), db.ExternalRootActionParams{
		BindingID: h.binding.ID, Actor: "operator", Reason: "operator_pause",
	})
	require.NoError(t, err)
	pausedBeforeAction, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	h.client.comments = []connector.Comment{{
		ID: "external-comment-1", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: h.now.Add(time.Second), UpdatedAt: h.now.Add(time.Second),
	}}

	resolved, event, err := NewService(h.store, h.registry, nil).ResolvePendingComment(
		t.Context(), ResolvePendingCommentParams{
			IssueID: h.issue.ID, Actor: "operator", Action: "adopt",
			ExternalCommentID: "external-comment-1",
		},
	)
	require.NoError(t, err)
	assert.False(t, resolved.Enabled)
	assert.True(t, resolved.Active)
	assert.Empty(t, resolved.ClaimToken)
	assert.Empty(t, resolved.PendingCommentUID)
	assert.Equal(t, pausedBeforeAction.LastAttemptAt, resolved.LastAttemptAt)
	assert.Equal(t, "issue.external_comment_resolved", event.Type)
}

type cancelAfterFieldResolutionStorage struct {
	db.Storage
	cancel        context.CancelFunc
	releaseCtxErr error
}

type refreshConflictAfterClaimStorage struct {
	db.Storage
	mappingID int64
	baseline  json.RawMessage
	kata      json.RawMessage
	external  json.RawMessage
}

func (s *refreshConflictAfterClaimStorage) ClaimExternalRootBindingForManualReconcile(
	ctx context.Context,
	bindingID int64,
	token string,
	now time.Time,
	staleBefore time.Time,
) (db.ExternalRootBinding, bool, error) {
	binding, acquired, err := s.Storage.ClaimExternalRootBindingForManualReconcile(ctx, bindingID, token, now, staleBefore)
	if err != nil || !acquired {
		return binding, acquired, err
	}
	_, _, err = s.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
		BindingID: binding.ID, MappingID: s.mappingID, ClaimToken: token,
		Baseline: s.baseline, ConflictKata: s.kata, ConflictExternal: s.external,
		Conflicted: true, At: now, Actor: "connector:notes",
	})
	return binding, acquired, err
}

func (s *cancelAfterFieldResolutionStorage) ResolveExternalFieldConflict(
	ctx context.Context,
	params db.ResolveExternalFieldConflictParams,
) (db.ExternalFieldState, db.Event, error) {
	state, event, err := s.Storage.ResolveExternalFieldConflict(ctx, params)
	if err == nil {
		s.cancel()
	}
	return state, event, err
}

func (s *cancelAfterFieldResolutionStorage) ReleaseExternalRootClaim(
	ctx context.Context,
	bindingID int64,
	token string,
) (db.ExternalRootBinding, error) {
	s.releaseCtxErr = ctx.Err()
	return s.Storage.ReleaseExternalRootClaim(ctx, bindingID, token)
}

func prepareFieldConflict(t *testing.T) (*reconcileHarness, *Service) {
	t.Helper()
	h := newReconcileHarness(t)
	h.mapField(t, "scheduled_on", "start-date")
	h.seedBaseline(t, "scheduled_on", date("2026-08-20"))
	h.setKataField(t, "scheduled_on", date("2026-08-21"))
	h.client.fieldValues["start-date"] = date("2026-08-22")
	result, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, result.FieldConflicts)
	return h, NewService(h.store, h.registry, nil)
}

func TestResolvePendingCommentAdoptsExactSuppliedExternalComment(t *testing.T) {
	h := newServiceHarness(t)
	binding, local, pendingAt := h.preparePendingComment(t, "Already created externally")
	external := connector.Comment{
		ID: "external-comment-21", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: pendingAt.Add(time.Second), UpdatedAt: pendingAt.Add(time.Second),
	}
	h.client.comments = []connector.Comment{external, {
		ID: "external-comment-22", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: pendingAt.Add(2 * time.Second), UpdatedAt: pendingAt.Add(2 * time.Second),
	}}

	resolved, event, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
		IssueID: h.issue.ID, Actor: "tester", Action: "adopt", ExternalCommentID: external.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, binding.ID, resolved.ID)
	assert.Empty(t, resolved.PendingCommentUID)
	assert.Empty(t, resolved.ClaimToken)
	mapping, err := h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootPublishedCommentMappingSource(binding), "comment", external.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, local.ID, *mapping.CommentID)
	assert.Equal(t, "issue.external_comment_resolved", event.Type)
	assert.Equal(t, "tester", event.Actor)
	var payload db.ExternalRootAuditPayload
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	assert.Equal(t, "adopt", payload.Action)
	assert.Equal(t, local.UID, payload.PendingCommentUID)
	assert.NotContains(t, event.Payload, external.ID)
	assert.NotContains(t, event.Payload, local.Body)
	assert.NotContains(t, event.Payload, h.client.description.SelfActorID)
	assert.NotContains(t, event.Payload, binding.ExternalAccountKey)
}

type countingClaimRenewalStorage struct {
	db.Storage
	renewals atomic.Int64
}

func (s *countingClaimRenewalStorage) RenewExternalRootClaim(
	ctx context.Context,
	bindingID int64,
	token string,
	now time.Time,
) (db.ExternalRootBinding, error) {
	s.renewals.Add(1)
	return s.Storage.RenewExternalRootClaim(ctx, bindingID, token, now)
}

func TestResolvePendingCommentAdoptRenewsClaimDuringConnectorCalls(t *testing.T) {
	h := newServiceHarness(t)
	_, local, pendingAt := h.preparePendingComment(t, "Already created externally")
	external := connector.Comment{
		ID: "external-comment-slow", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: pendingAt, UpdatedAt: pendingAt,
	}
	h.client.comments = []connector.Comment{external}
	entered := make(chan struct{})
	release := make(chan struct{})
	h.client.beforeDescribeReturn = func() {
		close(entered)
		<-release
	}
	store := &countingClaimRenewalStorage{Storage: h.observed}
	service := NewService(store, h.registry, nil)
	service.claimStaleAfter = 30 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, _, err := service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
			IssueID: h.issue.ID, Actor: "tester", Action: "adopt", ExternalCommentID: external.ID,
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("adoption did not enter the connector call")
	}
	require.Eventually(t, func() bool { return store.renewals.Load() >= 2 }, time.Second, time.Millisecond)
	close(release)
	require.NoError(t, <-done)
}

func TestResolvePendingCommentAdoptsExplicitSelfCommentDespiteBodyOrTimeVariance(t *testing.T) {
	for _, test := range []struct {
		name      string
		localBody string
		mutate    func(*connector.Comment, time.Time)
	}{
		{name: "normalized body", localBody: "Line one\r\nLine two", mutate: func(comment *connector.Comment, _ time.Time) {
			comment.Body = "Line one\nLine two"
		}},
		{name: "different body", localBody: "Local pending body", mutate: func(comment *connector.Comment, _ time.Time) {
			comment.Body = "Provider body differs"
		}},
		{name: "provider clock far behind", localBody: "Pending body", mutate: func(comment *connector.Comment, pendingAt time.Time) {
			comment.CreatedAt = pendingAt.Add(-24 * time.Hour)
			comment.UpdatedAt = comment.CreatedAt
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			binding, local, pendingAt := h.preparePendingComment(t, test.localBody)
			external := connector.Comment{
				ID: "external-comment-explicit", Body: local.Body,
				Author:    connector.Actor{ID: h.client.description.SelfActorID},
				CreatedAt: pendingAt, UpdatedAt: pendingAt,
			}
			test.mutate(&external, pendingAt)
			h.client.comments = []connector.Comment{external}

			resolved, event, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
				IssueID: h.issue.ID, Actor: "tester", Action: "adopt", ExternalCommentID: external.ID,
			})
			require.NoError(t, err)
			assert.Equal(t, binding.ID, resolved.ID)
			assert.Empty(t, resolved.PendingCommentUID)
			assert.Equal(t, "adopt", pendingResolutionAction(t, event))
			mapping, err := h.store.ImportMappingBySource(
				t.Context(), h.project.ID, db.ExternalRootPublishedCommentMappingSource(binding), "comment", external.ID,
			)
			require.NoError(t, err)
			require.NotNil(t, mapping.CommentID)
			assert.Equal(t, local.ID, *mapping.CommentID)
		})
	}
}

func TestResolvePendingCommentAdoptDoesNotStealExistingExternalMapping(t *testing.T) {
	h := newServiceHarness(t)
	binding, local, pendingAt := h.preparePendingComment(t, "Already created externally")
	other := h.createServiceComment(t, "Existing mapped comment")
	issueID, otherID := h.issue.ID, other.ID
	_, err := h.store.UpsertImportMapping(t.Context(), db.ImportMappingParams{
		Source: db.ExternalRootCommentMappingSource(binding), ExternalID: "external-comment-mapped", ObjectType: "comment",
		ProjectID: h.project.ID, IssueID: &issueID, CommentID: &otherID,
	})
	require.NoError(t, err)
	h.client.comments = []connector.Comment{{
		ID: "external-comment-mapped", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: pendingAt, UpdatedAt: pendingAt,
	}}

	_, _, err = h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
		IssueID: h.issue.ID, Actor: "tester", Action: "adopt", ExternalCommentID: "external-comment-mapped",
	})
	assert.ErrorIs(t, err, db.ErrExternalRootValidation)
	retained, readErr := h.store.ExternalRootBindingByID(t.Context(), binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, local.UID, retained.PendingCommentUID)
	assert.Empty(t, retained.ClaimToken)
	mapping, readErr := h.store.ImportMappingBySource(
		t.Context(), h.project.ID, db.ExternalRootCommentMappingSource(binding), "comment", "external-comment-mapped",
	)
	require.NoError(t, readErr)
	require.NotNil(t, mapping.CommentID)
	assert.Equal(t, other.ID, *mapping.CommentID)
}

func TestResolvePendingCommentRetryAllowsLaterPublication(t *testing.T) {
	h := newServiceHarness(t)
	binding, local, pendingAt := h.preparePendingComment(t, "Try publication again")

	resolved, event, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
		IssueID: h.issue.ID, Actor: "tester", Action: "retry",
	})
	require.NoError(t, err)
	assert.Equal(t, local.UID, resolved.PendingCommentUID)
	require.NotNil(t, resolved.PendingCommentStartedAt)
	assert.Empty(t, resolved.ClaimToken)
	assert.Equal(t, "retry", pendingResolutionAction(t, event))
	assert.Zero(t, h.client.publishCalls)

	createdAt := pendingAt.Add(3 * time.Minute)
	h.client.publishResult = connector.Comment{
		ID: "external-comment-23", Revision: "revision-external-comment-23", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	reconciler := NewReconciler(h.store, h.registry, ReconcilerConfig{Now: func() time.Time {
		return pendingAt.Add(3 * time.Minute)
	}})
	_, err = reconciler.Run(t.Context(), binding.ID, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, h.client.publishCalls)
}

func TestResolvePendingCommentSkipClearsWithClosedAuditAndNoPublish(t *testing.T) {
	h := newServiceHarness(t)
	binding, local, pendingAt := h.preparePendingComment(t, "Do not publish this")

	resolved, event, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
		IssueID: h.issue.ID, Actor: "tester", Action: "skip",
	})
	require.NoError(t, err)
	assert.Empty(t, resolved.PendingCommentUID)
	assert.Empty(t, resolved.ClaimToken)
	assert.Equal(t, "skip", pendingResolutionAction(t, event))
	assert.Zero(t, h.client.publishCalls)
	assert.NotContains(t, event.Payload, local.Body)
	suppression, err := h.store.ImportMappingBySource(
		t.Context(), h.project.ID, "connector-skip:notes", "comment", local.UID,
	)
	require.NoError(t, err)
	require.NotNil(t, suppression.CommentID)
	assert.Equal(t, local.ID, *suppression.CommentID)

	createdAt := pendingAt.Add(2 * time.Minute)
	h.client.publishResult = connector.Comment{
		ID: "must-not-publish", Revision: "revision-must-not-publish", Body: local.Body,
		Author:    connector.Actor{ID: h.client.description.SelfActorID},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	reconciler := NewReconciler(h.store, h.registry, ReconcilerConfig{Now: func() time.Time {
		return pendingAt.Add(3 * time.Minute)
	}})
	for range 2 {
		_, err = reconciler.Run(t.Context(), binding.ID, RunOptions{})
		require.NoError(t, err)
	}
	assert.Zero(t, h.client.publishCalls)
}

func TestResolvePendingCommentSkipAndRetryDoNotRequireConnector(t *testing.T) {
	for _, action := range []string{"skip", "retry"} {
		t.Run(action, func(t *testing.T) {
			h := newServiceHarness(t)
			binding, local, _ := h.preparePendingComment(t, "Resolve without connector")
			service := NewService(h.store, nil, nil)

			resolved, event, err := service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
				IssueID: h.issue.ID, Actor: "tester", Action: action,
			})

			require.NoError(t, err)
			assert.Equal(t, binding.ID, resolved.ID)
			if action == "retry" {
				assert.Equal(t, local.UID, resolved.PendingCommentUID)
			} else {
				assert.Empty(t, resolved.PendingCommentUID)
			}
			assert.Empty(t, resolved.ClaimToken)
			assert.Equal(t, action, pendingResolutionAction(t, event))
			assert.Zero(t, h.client.describeCalls)
			assert.Zero(t, h.client.readCalls)
			assert.Zero(t, h.client.listCommentCalls)
			if action == "skip" {
				mapping, readErr := h.store.ImportMappingBySource(
					t.Context(), h.project.ID, "connector-skip:notes", "comment", local.UID,
				)
				require.NoError(t, readErr)
				require.NotNil(t, mapping.CommentID)
				assert.Equal(t, local.ID, *mapping.CommentID)
			}
		})
	}
}

func TestResolvePendingCommentAdoptRejectsNonExactExternalComment(t *testing.T) {
	for _, test := range []struct {
		name       string
		suppliedID string
		mutate     func(*connector.Comment, time.Time)
	}{
		{name: "nonexact supplied ID", suppliedID: " external-comment-24 ", mutate: func(_ *connector.Comment, _ time.Time) {}},
		{name: "missing supplied ID", mutate: func(comment *connector.Comment, _ time.Time) {
			comment.ID = "different-comment"
		}},
		{name: "different author", mutate: func(comment *connector.Comment, _ time.Time) {
			comment.Author.ID = "different-actor"
		}},
		{name: "deleted", mutate: func(comment *connector.Comment, _ time.Time) {
			comment.Deleted = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			binding, local, pendingAt := h.preparePendingComment(t, "Exact pending body")
			external := connector.Comment{
				ID: "external-comment-24", Body: local.Body,
				Author:    connector.Actor{ID: h.client.description.SelfActorID},
				CreatedAt: pendingAt, UpdatedAt: pendingAt,
			}
			test.mutate(&external, pendingAt)
			h.client.comments = []connector.Comment{external}
			suppliedID := test.suppliedID
			if suppliedID == "" {
				suppliedID = "external-comment-24"
			}

			_, _, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
				IssueID: h.issue.ID, Actor: "tester", Action: "adopt", ExternalCommentID: suppliedID,
			})
			assert.ErrorIs(t, err, ErrPendingCommentResolutionRequired)
			retained, readErr := h.store.ExternalRootBindingByID(t.Context(), binding.ID)
			require.NoError(t, readErr)
			assert.Equal(t, local.UID, retained.PendingCommentUID)
			assert.Empty(t, retained.ClaimToken)
		})
	}
}

func TestResolvePendingCommentRevalidatesPublishingIdentityUnderClaim(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*serviceHarness)
		wantErr error
	}{
		{name: "publishing removed", mutate: func(h *serviceHarness) {
			h.client.description.Capabilities = nil
		}, wantErr: ErrCommentPublishingUnavailable},
		{name: "self actor removed", mutate: func(h *serviceHarness) {
			h.client.description.SelfActorID = ""
		}, wantErr: ErrCommentPublishingUnavailable},
		{name: "root identity changed", mutate: func(h *serviceHarness) {
			h.client.read.IdentityKey = "different-account"
		}, wantErr: ErrConnectorIdentityChanged},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newServiceHarness(t)
			binding, local, pendingAt := h.preparePendingComment(t, "Retain pending")
			h.client.comments = []connector.Comment{{
				ID: "external-comment-revalidated", Body: local.Body,
				Author:    connector.Actor{ID: h.client.description.SelfActorID},
				CreatedAt: pendingAt, UpdatedAt: pendingAt,
			}}
			test.mutate(h)

			_, _, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
				IssueID: h.issue.ID, Actor: "tester", Action: "adopt",
				ExternalCommentID: "external-comment-revalidated",
			})
			assert.ErrorIs(t, err, test.wantErr)
			retained, readErr := h.store.ExternalRootBindingByID(t.Context(), binding.ID)
			require.NoError(t, readErr)
			assert.Equal(t, local.UID, retained.PendingCommentUID)
			assert.Empty(t, retained.ClaimToken)
		})
	}
}

func TestResolvePendingCommentRejectsInvalidActionBeforeConnectorCalls(t *testing.T) {
	h := newServiceHarness(t)
	_, _, _ = h.preparePendingComment(t, "Retain pending")
	describeCalls, readCalls := h.client.describeCalls, h.client.readCalls

	_, _, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
		IssueID: h.issue.ID, Actor: "tester", Action: "unknown",
	})
	assert.ErrorIs(t, err, db.ErrExternalRootValidation)
	assert.Equal(t, describeCalls, h.client.describeCalls)
	assert.Equal(t, readCalls, h.client.readCalls)
}

func TestResolvePendingCommentReleasesClaimAfterCallerCancellation(t *testing.T) {
	h := newServiceHarness(t)
	binding, local, _ := h.preparePendingComment(t, "Retain after cancellation")
	ctx, cancel := context.WithCancel(t.Context())
	h.client.beforeReadReturn = cancel
	h.client.readErr = context.Canceled

	_, _, err := h.service.ResolvePendingComment(ctx, ResolvePendingCommentParams{
		IssueID: h.issue.ID, Actor: "tester", Action: "adopt",
		ExternalCommentID: "external-comment-canceled",
	})
	assert.ErrorIs(t, err, context.Canceled)
	retained, readErr := h.store.ExternalRootBindingByID(t.Context(), binding.ID)
	require.NoError(t, readErr)
	assert.Equal(t, local.UID, retained.PendingCommentUID)
	assert.Empty(t, retained.ClaimToken)
}

func TestResolvePendingCommentReturnsCommittedEventWhenClaimReleaseReportsError(t *testing.T) {
	h := newServiceHarness(t)
	binding, _, _ := h.preparePendingComment(t, "Skip with release error")
	releaseErr := errors.New("claim release result unavailable")
	h.service.store = &releaseThenFailStorage{Storage: h.store, err: releaseErr}

	resolved, event, err := h.service.ResolvePendingComment(t.Context(), ResolvePendingCommentParams{
		IssueID: h.issue.ID, Actor: "tester", Action: "skip",
	})
	assert.ErrorIs(t, err, releaseErr)
	assert.Equal(t, binding.ID, resolved.ID)
	assert.Equal(t, "issue.external_comment_resolved", event.Type)
	stored, readErr := h.store.ExternalRootBindingByID(t.Context(), binding.ID)
	require.NoError(t, readErr)
	assert.Empty(t, stored.PendingCommentUID)
	assert.Empty(t, stored.ClaimToken)
}

type releaseThenFailStorage struct {
	db.Storage
	err error
}

func (s *releaseThenFailStorage) ReleaseExternalRootClaim(
	ctx context.Context,
	bindingID int64,
	token string,
) (db.ExternalRootBinding, error) {
	binding, err := s.Storage.ReleaseExternalRootClaim(ctx, bindingID, token)
	if err != nil {
		return binding, err
	}
	return binding, s.err
}

func (h *serviceHarness) preparePendingComment(
	t *testing.T,
	body string,
) (db.ExternalRootBinding, db.Comment, time.Time) {
	t.Helper()
	h.client.read.UpdatedAt = testObservedAt
	frontier := testObservedAt
	binding, _, err := h.store.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: h.project.ID, IssueID: h.issue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-1", ExternalAccountKey: "account-1",
		Actor: "tester", ReceiveCommentsAfter: testObservedAt,
		PublishComments: true, PublishCommentsAfter: &frontier,
	})
	require.NoError(t, err)
	local := h.createServiceComment(t, body)
	pendingAt := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	claimed, acquired, err := h.store.ClaimExternalRootBinding(
		t.Context(), binding.ID, "pending-setup-claim", pendingAt, pendingAt.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = h.store.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: claimed.ClaimToken, CommentUID: local.UID, At: pendingAt,
	})
	require.NoError(t, err)
	_, err = h.store.RecordExternalRootError(t.Context(), db.ExternalRootErrorParams{
		BindingID: binding.ID, ClaimToken: claimed.ClaimToken,
		At: pendingAt, NextAttemptAt: pendingAt, Error: "publication result uncertain",
	})
	require.NoError(t, err)
	return binding, local, pendingAt
}

func pendingResolutionAction(t *testing.T, event db.Event) string {
	t.Helper()
	var payload db.ExternalRootAuditPayload
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	return payload.Action
}

type serviceHarness struct {
	store    *sqlitestore.Store
	observed *observingStorage
	project  db.Project
	issue    db.Issue
	client   *fakeConnectorClient
	registry *Registry
	service  *Service
}

func newServiceHarness(t *testing.T) *serviceHarness {
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
	client := &fakeConnectorClient{
		description: testDescription("account-1"),
		resolved:    connector.Root{Key: "root-1", IdentityKey: "account-1", Title: "External plan", Body: "External body", State: "open", Revision: "7", ObservedAt: testObservedAt},
		read:        connector.Root{Key: "root-1", IdentityKey: "account-1", Title: "External plan", Body: "External body", State: "open", Revision: "7", ObservedAt: testObservedAt},
	}
	client.description.Capabilities = []connector.Capability{
		connector.CapabilityPublishComment,
		connector.CapabilityFields,
		connector.CapabilityConditionalFields,
	}
	client.description.SelfActorID = "connector-self"
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	observed := &observingStorage{Storage: store}
	service := NewService(observed, registry, func(ctx context.Context, bindingID int64, claimToken string) ([]db.Event, error) {
		_, err := store.ReleaseExternalRootClaim(ctx, bindingID, claimToken)
		return nil, err
	})
	return &serviceHarness{store: store, observed: observed, project: project, issue: issue, client: client, registry: registry, service: service}
}

type observingStorage struct {
	db.Storage
	hookFinished         bool
	issueReadAfterHook   bool
	bindingReadAfterHook bool
}

func (s *observingStorage) IssueByID(ctx context.Context, issueID int64) (db.Issue, error) {
	if s.hookFinished {
		s.issueReadAfterHook = true
	}
	return s.Storage.IssueByID(ctx, issueID)
}

func (s *observingStorage) ExternalRootBindingByID(ctx context.Context, bindingID int64) (db.ExternalRootBinding, error) {
	if s.hookFinished {
		s.bindingReadAfterHook = true
	}
	return s.Storage.ExternalRootBindingByID(ctx, bindingID)
}

type fakeConnectorClient struct {
	description          connector.Description
	describeErr          error
	beforeDescribeReturn func()
	resolved             connector.Root
	resolveErr           error
	read                 connector.Root
	readErr              error
	beforeReadReturn     func()
	comments             []connector.Comment
	fields               []connector.FieldDescriptor
	fieldValues          map[string]connector.FieldValue
	listFieldsErr        error
	readFieldsErr        map[string]error
	writeFieldsErr       error
	writeFieldResult     func(string, connector.FieldValue) connector.FieldValue
	beforeWriteFields    func()
	listFieldsCalls      int
	readFieldCalls       []connector.ReadFieldsParams
	writeFieldCalls      []connector.WriteFieldsParams
	describeCalls        int
	resolveCalls         int
	readCalls            int
	listCommentCalls     int
	publishCalls         int
	publishParams        []connector.PublishCommentParams
	publishResult        connector.Comment
	publishErr           error
	beforePublish        func()
	completeCalls        int
	completeErr          error
	completeReadback     connector.Root
	beforeComplete       func()
}

func (c *fakeConnectorClient) Describe(context.Context) (connector.Description, error) {
	c.describeCalls++
	if c.beforeDescribeReturn != nil {
		c.beforeDescribeReturn()
	}
	return c.description, c.describeErr
}
func (c *fakeConnectorClient) ResolveRoot(context.Context, connector.ResolveRootParams) (connector.Root, error) {
	c.resolveCalls++
	return c.resolved, c.resolveErr
}
func (c *fakeConnectorClient) ReadRoot(context.Context, connector.ReadRootParams) (connector.Root, error) {
	c.readCalls++
	if c.beforeReadReturn != nil {
		c.beforeReadReturn()
	}
	return c.read, c.readErr
}
func (c *fakeConnectorClient) ListComments(context.Context, connector.ListCommentsParams) (connector.ListCommentsResult, error) {
	c.listCommentCalls++
	comments := append([]connector.Comment(nil), c.comments...)
	for index := range comments {
		if comments[index].Revision == "" {
			comments[index].Revision = fmt.Sprintf("test-comment-revision-%d", index)
		}
	}
	return connector.ListCommentsResult{Comments: comments}, nil
}
func (c *fakeConnectorClient) CompleteRoot(context.Context, connector.CompleteRootParams) (connector.Root, error) {
	c.completeCalls++
	if c.beforeComplete != nil {
		c.beforeComplete()
	}
	if c.completeErr != nil {
		return connector.Root{}, c.completeErr
	}
	if c.completeReadback.Key != "" {
		c.read = c.completeReadback
	}
	return c.read, nil
}
func (c *fakeConnectorClient) PublishComment(_ context.Context, params connector.PublishCommentParams) (connector.Comment, error) {
	c.publishCalls++
	c.publishParams = append(c.publishParams, params)
	if c.beforePublish != nil {
		c.beforePublish()
	}
	return c.publishResult, c.publishErr
}
func (c *fakeConnectorClient) ListFields(context.Context) (connector.ListFieldsResult, error) {
	c.listFieldsCalls++
	return connector.ListFieldsResult{Fields: c.fields}, c.listFieldsErr
}
func (c *fakeConnectorClient) ReadFields(_ context.Context, params connector.ReadFieldsParams) (connector.ReadFieldsResult, error) {
	c.readFieldCalls = append(c.readFieldCalls, params)
	result := connector.ReadFieldsResult{Fields: make(map[string]connector.FieldValue, len(params.FieldIDs))}
	for _, fieldID := range params.FieldIDs {
		if err := c.readFieldsErr[fieldID]; err != nil {
			return connector.ReadFieldsResult{}, err
		}
		if value, ok := c.fieldValues[fieldID]; ok {
			result.Fields[fieldID] = value
		}
	}
	return result, nil
}
func (c *fakeConnectorClient) WriteFields(_ context.Context, params connector.WriteFieldsParams) (connector.WriteFieldsResult, error) {
	c.writeFieldCalls = append(c.writeFieldCalls, params)
	if c.beforeWriteFields != nil {
		c.beforeWriteFields()
	}
	if c.writeFieldsErr != nil {
		return connector.WriteFieldsResult{}, c.writeFieldsErr
	}
	if c.fieldValues == nil {
		c.fieldValues = make(map[string]connector.FieldValue)
	}
	for fieldID := range params.Fields {
		if expected, ok := params.Expected[fieldID]; !ok || c.fieldValues[fieldID] != expected {
			return connector.WriteFieldsResult{}, &connector.Error{Code: "field_conflict", Message: "field changed before write"}
		}
	}
	result := connector.WriteFieldsResult{Fields: make(map[string]connector.FieldValue, len(params.Fields))}
	for fieldID, value := range params.Fields {
		stored := value
		if c.writeFieldResult != nil {
			stored = c.writeFieldResult(fieldID, value)
		}
		c.fieldValues[fieldID] = stored
		result.Fields[fieldID] = stored
	}
	return result, nil
}
