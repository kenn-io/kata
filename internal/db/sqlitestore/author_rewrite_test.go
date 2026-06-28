package sqlitestore_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRewriteAuthorIdentity_ScopedCountsAndIdempotent(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	other := createProject(ctx, t, d, "other-project")
	from, to := "old-agent", "new-agent"

	owned := from
	first, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "first",
		Author:    from,
		Owner:     &owned,
	})
	require.NoError(t, err)
	second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "second",
		Author:    "keep-agent",
	})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: first.ID,
		Author:  from,
		Body:    "rewrite comment author",
	})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: second.ID,
		Author:  "keep-agent",
		Body:    "keep comment author",
	})
	require.NoError(t, err)
	link, err := d.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: first.ID,
		ToIssueID:   second.ID,
		Type:        "blocks",
		Author:      from,
	})
	require.NoError(t, err)
	otherIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: other.ID,
		Title:     "other",
		Author:    from,
	})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: otherIssue.ID,
		Author:  from,
		Body:    "other project comment",
	})
	require.NoError(t, err)

	result, err := d.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: p.ID,
		Actor:     "operator",
		From:      from,
		To:        to,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.IssueAuthors)
	assert.Equal(t, int64(1), result.IssueOwners)
	assert.Equal(t, int64(1), result.CommentAuthors)
	assert.Equal(t, int64(1), result.LinkAuthors)
	require.NotNil(t, result.Event)
	assert.Equal(t, "project.author_rewritten", result.Event.Type)

	updatedFirst, err := d.IssueByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, to, updatedFirst.Author)
	require.NotNil(t, updatedFirst.Owner)
	assert.Equal(t, to, *updatedFirst.Owner)
	updatedSecond, err := d.IssueByID(ctx, second.ID)
	require.NoError(t, err)
	assert.Equal(t, "keep-agent", updatedSecond.Author)
	comments, err := d.CommentsByIssue(ctx, first.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, to, comments[0].Author)
	updatedLink, err := d.LinkByID(ctx, link.ID)
	require.NoError(t, err)
	assert.Equal(t, to, updatedLink.Author)
	otherAfter, err := d.IssueByID(ctx, otherIssue.ID)
	require.NoError(t, err)
	assert.Equal(t, from, otherAfter.Author)

	again, err := d.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: p.ID,
		Actor:     "operator",
		From:      from,
		To:        to,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), again.Total())
	assert.Nil(t, again.Event)
}

func TestRewriteAuthorIdentity_RejectsEmptyTo(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	_, err := d.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: p.ID,
		Actor:     "operator",
		From:      "old-agent",
		To:        "   ",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "to author is required")
}

func TestRewriteAuthorIdentity_ReplayKeepsRewrittenAuthors(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	from, to := "old-agent", "new-agent"
	owner := from
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "replay target",
		Author:    from,
		Owner:     &owner,
	})
	require.NoError(t, err)
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "replay peer",
		Author:    "peer-agent",
	})
	require.NoError(t, err)
	untouched, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "untouched",
		Author:    "keep-agent",
	})
	require.NoError(t, err)
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  from,
		Body:    "comment",
	})
	require.NoError(t, err)
	_, _, err = d.CreateLinkAndEvent(ctx,
		db.CreateLinkParams{
			FromIssueID: issue.ID,
			ToIssueID:   peer.ID,
			Type:        "blocks",
			Author:      from,
		},
		db.LinkEventParams{
			EventType:    "issue.linked",
			EventIssueID: issue.ID,
			FromShortID:  issue.ShortID,
			FromUID:      issue.UID,
			ToShortID:    peer.ShortID,
			ToUID:        peer.UID,
			Actor:        from,
		})
	require.NoError(t, err)

	_, err = d.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: p.ID,
		Actor:     "operator",
		From:      from,
		To:        to,
	})
	require.NoError(t, err)

	folded := db.FoldEvents(loadFoldEventsForProject(ctx, t, d, p.ID))
	foldedIssue := folded.Issues[issue.UID]
	assert.Equal(t, to, foldedIssue.Author)
	require.NotNil(t, foldedIssue.Owner)
	assert.Equal(t, to, *foldedIssue.Owner)
	foldedComment := folded.Comments[comment.UID]
	assert.Equal(t, to, foldedComment.Author)
	foldedLink := folded.Links[db.FoldLinkKey{FromUID: issue.UID, ToUID: peer.UID, Type: "blocks"}]
	assert.Equal(t, to, foldedLink.Author)
	assert.Equal(t,
		untouched.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		folded.Issues[untouched.UID].UpdatedAt,
	)
}

func TestRewriteAuthorIdentity_ReplayRewritesInitialLinkAuthors(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	from, to := "old-agent", "new-agent"
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "initial link peer",
		Author:    "peer-agent",
	})
	require.NoError(t, err)
	subject, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "initial link subject",
		Author:    from,
		Links: []db.InitialLink{{
			Type:     "blocks",
			ToNumber: peer.ID,
		}},
	})
	require.NoError(t, err)

	_, err = d.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: p.ID,
		Actor:     "operator",
		From:      from,
		To:        to,
	})
	require.NoError(t, err)

	folded := db.FoldEvents(loadFoldEventsForProject(ctx, t, d, p.ID))
	foldedLink := folded.Links[db.FoldLinkKey{FromUID: subject.UID, ToUID: peer.UID, Type: "blocks"}]
	assert.Equal(t, to, foldedLink.Author)
}

func TestRewriteAuthorIdentity_FederationBaselineSnapshotsRewrittenAuthors(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	from, to := "old-agent", "new-agent"
	owner := from
	subject, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "snapshot subject",
		Author:    from,
		Owner:     &owner,
	})
	require.NoError(t, err)
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "snapshot peer",
		Author:    "peer-agent",
	})
	require.NoError(t, err)
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: subject.ID,
		Author:  from,
		Body:    "snapshot comment",
	})
	require.NoError(t, err)
	_, err = d.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: subject.ID,
		ToIssueID:   peer.ID,
		Type:        "related",
		Author:      from,
	})
	require.NoError(t, err)

	_, err = d.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: p.ID,
		Actor:     "operator",
		From:      from,
		To:        to,
	})
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, err)

	events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 100})
	require.NoError(t, err)
	var snapshot db.Event
	for _, ev := range events {
		if ev.Type == "issue.snapshot" && ev.IssueUID != nil && *ev.IssueUID == subject.UID {
			snapshot = ev
			break
		}
	}
	require.NotEmpty(t, snapshot.UID)
	payload := unmarshalPayload[federationSnapshotPayload](t, snapshot.Payload)
	assert.Equal(t, to, payload.Author)
	require.NotNil(t, payload.Owner)
	assert.Equal(t, to, *payload.Owner)
	require.Len(t, payload.Comments, 1)
	assert.Equal(t, comment.UID, payload.Comments[0].CommentUID)
	assert.Equal(t, to, payload.Comments[0].Author)
	require.Len(t, payload.Links, 1)
	assert.Equal(t, to, payload.Links[0].Author)

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), from)
}
