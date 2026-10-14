package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestScopedCommentReplyTargetReparentedDuringProjectionFailsBufferedResponse(t *testing.T) {
	fixture := newLateReplyTargetFixture(t)
	principal, ok := PrincipalFromContext(fixture.ctx)
	require.True(t, ok)
	require.NotNil(t, principal.Scope)
	comments, err := fixture.store.CommentsByIssue(t.Context(), fixture.source.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, fixture.targetUID, comments[0].ReplyToUID)

	store := &reparentAfterMembershipReadStore{Storage: fixture.store, parentLinkID: fixture.parentLinkID}
	projected, _, scoped, err := projectScopedCommentReplies(fixture.ctx, store, comments)
	require.NoError(t, err)
	require.True(t, scoped)
	require.Equal(t, fixture.targetUID, projected[0].ReplyToUID)

	// Show and edit responses are buffered. Registering the retained target
	// lets the response guard reject it after the target leaves the subtree.
	err = validateScopedResponse(fixture.ctx, store, *principal.Scope)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 404, apiErr.Status)
}

type reparentAfterMembershipReadStore struct {
	db.Storage
	parentLinkID int64
	changed      bool
}

func (s *reparentAfterMembershipReadStore) IssueScopedMembers(
	ctx context.Context, scope db.APITokenScope,
) ([]db.Issue, error) {
	members, err := s.Storage.IssueScopedMembers(ctx, scope)
	if err != nil {
		return nil, err
	}
	if !s.changed {
		s.changed = true
		if err := s.DeleteLinkByID(ctx, s.parentLinkID); err != nil {
			return nil, err
		}
	}
	return members, nil
}
