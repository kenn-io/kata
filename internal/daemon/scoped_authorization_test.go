package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestScopedPrincipalTransactionFenceRejectsRevocationBeforeFirstWrite(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	principal, ok := PrincipalFromContext(ctx)
	require.True(t, ok)
	gated := &gatedIssueScopedFenceStore{
		Storage: store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	mutationErr := make(chan error, 1)
	handler := withScopedPrincipalRevalidation(gated, http.HandlerFunc(
		func(_ http.ResponseWriter, request *http.Request) {
			_, _, createErr := gated.CreateComment(request.Context(), db.CreateCommentParams{
				IssueID: root.ID, Body: "Must not land", Author: principal.Actor,
			})
			mutationErr <- internalAPIError(createErr)
		}))
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx))
		close(done)
	}()
	<-gated.entered
	_, _, err = store.RevokeAPIToken(t.Context(), principal.TokenID, db.BootstrapActor)
	require.NoError(t, err)
	close(gated.release)

	var apiErr *api.APIError
	require.ErrorAs(t, <-mutationErr, &apiErr)
	require.Equal(t, http.StatusUnauthorized, apiErr.Status)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fenced mutation did not return")
	}
	comments, err := store.CommentsByIssue(t.Context(), root.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

func TestScopedMutationRechecksTargetMembershipInsideTransaction(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	child := createScopedAuthIssue(t, store, project.ID, "Child", &root)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	handler := withScopedPrincipalRevalidation(store, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		require.NoError(t, authorizeIssueScopedIssue(request.Context(), store, child))
		parent, err := store.ParentOf(t.Context(), child.ID)
		require.NoError(t, err)
		require.NoError(t, store.DeleteLinkByID(t.Context(), parent.ID))
		_, _, err = store.CreateComment(request.Context(), db.CreateCommentParams{
			IssueID: child.ID, Body: "Must not land", Author: "worker-a",
		})
		require.Error(t, err)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx))
	comments, err := store.CommentsByIssue(t.Context(), child.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

type gatedIssueScopedFenceStore struct {
	db.Storage
	entered chan struct{}
	release chan struct{}
}

func (s *gatedIssueScopedFenceStore) IssueScopedTokenTransactionFence(
	admitted db.APIToken,
) db.TransactionFence {
	native := s.Storage.IssueScopedTokenTransactionFence(admitted)
	return func(ctx context.Context, transaction db.Transaction) error {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return native(ctx, transaction)
	}
}

func TestIssueScopedAuthorizationFollowsOnlyActiveParentContainment(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	otherProject, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	child := createScopedAuthIssue(t, store, project.ID, "Child", &root)
	grandchild := createScopedAuthIssue(t, store, project.ID, "Grandchild", &child)
	outside := createScopedAuthIssue(t, store, project.ID, "Outside", nil)
	foreignChild := createScopedAuthIssue(t, store, otherProject.ID, "Foreign", &root)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)

	for _, issue := range []db.Issue{root, child, grandchild} {
		require.NoError(t, authorizeIssueScopedIssue(ctx, store, issue))
	}
	require.Error(t, authorizeIssueScopedIssue(ctx, store, outside))
	require.Error(t, authorizeIssueScopedIssue(ctx, store, foreignChild))
	require.NoError(t, authorizeIssueScopedProject(ctx, project))
	require.Error(t, authorizeIssueScopedProject(ctx, otherProject))
}

func TestScopedReportPayloadFailsClosedWhenSafePayloadIsNotJSONObject(t *testing.T) {
	allowedUIDs := map[string]struct{}{"peer-issue-uid": {}}

	// A JSON null safe payload unmarshals without error but leaves the
	// projection map nil; the enrichment write must fail closed to the safe
	// payload instead of panicking.
	createdRaw := `{"links":[{"type":"parent","to_short_id":"peer-1","to_issue_uid":"peer-issue-uid","incoming":false}]}`
	require.NotPanics(t, func() {
		require.Equal(t, "null", scopedCreatedReportPayload(createdRaw, "null", allowedUIDs))
	})

	closedRaw := `{"parent_uid":"peer-issue-uid","parent_short_id":"peer-1"}`
	require.NotPanics(t, func() {
		require.Equal(t, "null", scopedClosedReportPayload(closedRaw, "null", allowedUIDs))
	})
}

func TestScopedEventVisibilityRechecksMembershipAfterSelection(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	child := createScopedAuthIssue(t, store, project.ID, "Child", &root)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	event := db.Event{IssueID: &child.ID, ProjectUID: project.UID, Type: "issue.updated"}

	visible, err := scopedEventStillVisible(ctx, store, event)
	require.NoError(t, err)
	require.True(t, visible)
	parent, err := store.ParentOf(t.Context(), child.ID)
	require.NoError(t, err)
	require.NoError(t, store.DeleteLinkByID(t.Context(), parent.ID))
	visible, err = scopedEventStillVisible(ctx, store, event)
	require.NoError(t, err)
	require.False(t, visible)
}

func withScopedAuthorizationTestPrincipal(
	t *testing.T, store db.Storage, project db.Project, root db.Issue,
) context.Context {
	t.Helper()
	expiresAt := time.Now().UTC().Add(time.Hour)
	token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "scoped-authorization-test-token", Actor: "worker-a",
		AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	return WithPrincipal(context.Background(), Principal{
		Kind: PrincipalDBToken, Actor: token.Actor, TokenID: token.ID,
		Scope: token.Scope, ExpiresAt: token.ExpiresAt,
	})
}

func createScopedAuthIssue(t *testing.T, store db.Storage, projectID int64, title string, parent *db.Issue) db.Issue {
	t.Helper()
	params := db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "coordinator"}
	if parent != nil {
		params.Links = []db.InitialLink{{Type: "parent", ToNumber: parent.ID}}
	}
	issue, _, err := store.CreateIssue(t.Context(), params)
	require.NoError(t, err)
	return issue
}
