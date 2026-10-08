package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestScopedReplyTargetReparentedDuringProjectionFailsBufferedResponse(t *testing.T) {
	fixture := newLateReplyTargetFixture(t)
	principal, ok := PrincipalFromContext(fixture.ctx)
	require.True(t, ok)
	require.NotNil(t, principal.Scope)
	allowed, _, err := issueScopedEventMembership(fixture.ctx, fixture.store, *principal.Scope)
	require.NoError(t, err)

	store := &reparentAfterReplyLookupStore{Storage: fixture.store, parentLinkID: fixture.parentLinkID}
	projected, visible, err := projectIssueScopedEvents(
		fixture.ctx, store, []db.Event{fixture.event}, allowed, fixture.project.UID,
	)
	require.NoError(t, err)
	require.Equal(t, []bool{true}, visible)
	require.Contains(t, projected[0].Payload, fixture.targetUID)

	// Poll and snapshot responses are buffered. The response guard must know
	// about the target resolved during projection and reject it after its move.
	err = validateScopedResponse(fixture.ctx, store, *principal.Scope)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 404, apiErr.Status)
}

func TestScopedSSERechecksLateReplyTargetAfterProjection(t *testing.T) {
	fixture := newLateReplyTargetFixture(t)
	principal, ok := PrincipalFromContext(fixture.ctx)
	require.True(t, ok)
	require.NotNil(t, principal.Scope)
	allowed, _, err := issueScopedEventMembership(fixture.ctx, fixture.store, *principal.Scope)
	require.NoError(t, err)

	store := &reparentAfterReplyLookupStore{Storage: fixture.store, parentLinkID: fixture.parentLinkID}
	projected, visible, err := projectIssueScopedEvents(
		fixture.ctx, store, []db.Event{fixture.event}, allowed, fixture.project.UID,
	)
	require.NoError(t, err)
	require.Equal(t, []bool{true}, visible)
	require.Contains(t, projected[0].Payload, fixture.targetUID)
	// The event has no related-issue envelope because its target arrived after
	// event creation. SSE must resolve the payload target again before writing.
	require.Nil(t, projected[0].RelatedIssueID)

	stillVisible, err := scopedEventStillVisible(fixture.ctx, store, projected[0])
	require.NoError(t, err)
	require.False(t, stillVisible)
}

type lateReplyTargetFixture struct {
	store        *sqlitestore.Store
	ctx          context.Context
	project      db.Project
	source       db.Issue
	event        db.Event
	parentLinkID int64
	targetUID    string
}

func newLateReplyTargetFixture(t *testing.T) lateReplyTargetFixture {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	source := createScopedAuthIssue(t, store, project.ID, "Reply source", &root)
	targetIssue := createScopedAuthIssue(t, store, project.ID, "Reply target", &root)
	parent, err := store.ParentOf(t.Context(), targetIssue.ID)
	require.NoError(t, err)
	targetUID := "01EEEEEEEEEEEEEEEEEEEEEEEE"
	_, event, err := store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: source.ID, Author: "worker-a", Body: "Reply before target",
		ReplyToUID: targetUID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	require.Nil(t, event.RelatedIssueID)
	require.Nil(t, event.RelatedIssueUID)
	_, err = store.ExecContext(t.Context(),
		`INSERT INTO comments(uid, issue_id, author, body) VALUES (?, ?, ?, ?)`,
		targetUID, targetIssue.ID, "worker-a", "Late target comment")
	require.NoError(t, err)
	ctx := db.WithIssueScopeTargets(withScopedAuthorizationTestPrincipal(t, store, project, root))
	return lateReplyTargetFixture{
		store: store, ctx: ctx, project: project, source: source, event: event,
		parentLinkID: parent.ID, targetUID: targetUID,
	}
}

type reparentAfterReplyLookupStore struct {
	db.Storage
	parentLinkID int64
	changed      bool
}

func (s *reparentAfterReplyLookupStore) CommentIssueIDsByUIDs(
	ctx context.Context, uids []string,
) (map[string]int64, error) {
	owners, err := s.Storage.CommentIssueIDsByUIDs(ctx, uids)
	if err != nil {
		return nil, err
	}
	if !s.changed {
		s.changed = true
		if err := s.DeleteLinkByID(ctx, s.parentLinkID); err != nil {
			return nil, err
		}
	}
	return owners, nil
}
