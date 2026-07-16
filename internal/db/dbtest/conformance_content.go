package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func createIssueFixture(
	ctx context.Context,
	store db.Storage,
	projectName string,
	title string,
	author string,
	owner *string,
) (issueFixture, error) {
	project, err := store.CreateProject(ctx, projectName)
	if err != nil {
		return issueFixture{}, fmt.Errorf("create fixture project: %w", err)
	}
	issue, err := createFixtureIssue(ctx, store, project.ID, title, author, owner)
	if err != nil {
		return issueFixture{}, err
	}
	return issueFixture{Project: project, Issue: issue}, nil
}

func createFixtureIssue(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	title string,
	author string,
	owner *string,
) (db.Issue, error) {
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectID,
		Title:     title,
		Author:    author,
		Owner:     owner,
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create fixture issue: %w", err)
	}
	return issue, nil
}

func checkComments(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	originalOwner := "original-author"
	primary, err := createIssueFixture(
		ctx, store, "comment-project", "comment behavior", "original-author", &originalOwner,
	)
	if err != nil {
		return err
	}

	first, createdEvent, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: primary.Issue.ID,
		Author:  "original-author",
		Body:    "first body",
	})
	if err != nil {
		return fmt.Errorf("create first comment: %w", err)
	}
	assert.NotZero(t, first.ID)
	assert.True(t, uid.Valid(first.UID))
	assert.Equal(t, primary.Issue.ID, first.IssueID)
	assert.Equal(t, "original-author", first.Author)
	assert.Equal(t, "first body", first.Body)
	assert.False(t, first.CreatedAt.IsZero())
	assert.Equal(t, "issue.commented", createdEvent.Type)
	assert.Equal(t, "original-author", createdEvent.Actor)
	require.NotNil(t, createdEvent.IssueUID)
	assert.Equal(t, primary.Issue.UID, *createdEvent.IssueUID)
	var createdPayload struct {
		CommentUID string `json:"comment_uid"`
		Author     string `json:"author"`
		Body       string `json:"body"`
	}
	require.NoError(t, json.Unmarshal([]byte(createdEvent.Payload), &createdPayload))
	assert.Equal(t, first.UID, createdPayload.CommentUID)
	assert.Equal(t, "original-author", createdPayload.Author)
	assert.Equal(t, "first body", createdPayload.Body)

	second, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: primary.Issue.ID,
		Author:  "original-author",
		Body:    "second body",
	})
	if err != nil {
		return fmt.Errorf("create second comment: %w", err)
	}
	comments, err := store.CommentsByIssue(ctx, primary.Issue.ID)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	require.Len(t, comments, 2)
	assert.Equal(t, []int64{first.ID, second.ID}, []int64{comments[0].ID, comments[1].ID})

	updated, editedEvent, changed, err := store.EditComment(ctx, db.EditCommentParams{
		IssueID:    primary.Issue.ID,
		CommentUID: first.UID,
		Actor:      "comment-editor",
		Body:       "edited body",
	})
	if err != nil {
		return fmt.Errorf("edit comment: %w", err)
	}
	assert.True(t, changed)
	require.NotNil(t, editedEvent)
	assert.Equal(t, "issue.comment_edited", editedEvent.Type)
	assert.Equal(t, "comment-editor", editedEvent.Actor)
	assert.Equal(t, "edited body", updated.Body)
	assert.Equal(t, first.Author, updated.Author)
	assert.Equal(t, first.CreatedAt, updated.CreatedAt)
	body, err := store.CommentBodyByID(ctx, first.ID)
	if err != nil {
		return fmt.Errorf("comment body by ID: %w", err)
	}
	assert.Equal(t, "edited body", body)

	unchanged, noEvent, changed, err := store.EditComment(ctx, db.EditCommentParams{
		IssueID:    primary.Issue.ID,
		CommentUID: first.UID,
		Actor:      "comment-editor",
		Body:       "edited body",
	})
	if err != nil {
		return fmt.Errorf("repeat comment edit: %w", err)
	}
	assert.False(t, changed)
	assert.Nil(t, noEvent)
	assert.Equal(t, updated, unchanged)
	_, _, _, err = store.EditComment(ctx, db.EditCommentParams{
		IssueID: primary.Issue.ID, CommentUID: first.UID, Actor: "comment-editor", Body: " ",
	})
	assert.Error(t, err)
	_, _, _, err = store.EditComment(ctx, db.EditCommentParams{
		IssueID: primary.Issue.ID, CommentUID: "missing-comment", Actor: "comment-editor", Body: "replacement",
	})
	assert.ErrorIs(t, err, db.ErrNotFound)

	other, err := createIssueFixture(ctx, store, "other-comment-project", "scoped comment", "original-author", nil)
	if err != nil {
		return err
	}
	otherComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: other.Issue.ID, Author: "original-author", Body: "outside scope",
	})
	if err != nil {
		return fmt.Errorf("create out-of-scope comment: %w", err)
	}
	rewrite, err := store.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: primary.Project.ID,
		Actor:     "project-manager",
		From:      "original-author",
		To:        "renamed-author",
	})
	if err != nil {
		return fmt.Errorf("rewrite author identity: %w", err)
	}
	assert.True(t, rewrite.Changed)
	assert.Equal(t, int64(1), rewrite.IssueAuthors)
	assert.Equal(t, int64(1), rewrite.IssueOwners)
	assert.Equal(t, int64(2), rewrite.CommentAuthors)
	assert.Equal(t, int64(0), rewrite.LinkAuthors)
	assert.Equal(t, int64(4), rewrite.Total())
	require.NotNil(t, rewrite.Event)
	assert.Equal(t, "project.author_rewritten", rewrite.Event.Type)

	rewrittenIssue, err := store.IssueByID(ctx, primary.Issue.ID)
	if err != nil {
		return fmt.Errorf("read rewritten issue: %w", err)
	}
	assert.Equal(t, "renamed-author", rewrittenIssue.Author)
	require.NotNil(t, rewrittenIssue.Owner)
	assert.Equal(t, "renamed-author", *rewrittenIssue.Owner)
	comments, err = store.CommentsByIssue(ctx, primary.Issue.ID)
	if err != nil {
		return fmt.Errorf("list rewritten comments: %w", err)
	}
	require.Len(t, comments, 2)
	assert.Equal(t, "renamed-author", comments[0].Author)
	assert.Equal(t, "renamed-author", comments[1].Author)
	outsideComments, err := store.CommentsByIssue(ctx, other.Issue.ID)
	if err != nil {
		return fmt.Errorf("list out-of-scope comments: %w", err)
	}
	require.Len(t, outsideComments, 1)
	assert.Equal(t, otherComment.Author, outsideComments[0].Author)

	noRewrite, err := store.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
		ProjectID: primary.Project.ID,
		Actor:     "project-manager",
		From:      "absent-author",
		To:        "renamed-author",
	})
	if err != nil {
		return fmt.Errorf("repeat author rewrite: %w", err)
	}
	assert.False(t, noRewrite.Changed)
	assert.Zero(t, noRewrite.Total())
	assert.Nil(t, noRewrite.Event)
	return nil
}

func checkLabels(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	fixture, err := createIssueFixture(ctx, store, "label-project", "first labeled issue", "label-author", nil)
	if err != nil {
		return err
	}
	second, err := createFixtureIssue(ctx, store, fixture.Project.ID, "second labeled issue", "label-author", nil)
	if err != nil {
		return err
	}

	priority, err := store.AddLabel(ctx, fixture.Issue.ID, "priority:high", "label-author")
	if err != nil {
		return fmt.Errorf("add direct label: %w", err)
	}
	assert.Equal(t, fixture.Issue.ID, priority.IssueID)
	assert.Equal(t, "priority:high", priority.Label)
	assert.Equal(t, "label-author", priority.Author)
	assert.False(t, priority.CreatedAt.IsZero())
	_, err = store.AddLabel(ctx, fixture.Issue.ID, "priority:high", "label-author")
	assert.ErrorIs(t, err, db.ErrLabelExists)
	for _, invalid := range []string{"", "UPPER", "with space", "invalid!"} {
		_, err = store.AddLabel(ctx, fixture.Issue.ID, invalid, "label-author")
		assert.ErrorIs(t, err, db.ErrLabelInvalid, "label=%q", invalid)
	}
	_, err = store.AddLabel(ctx, fixture.Issue.ID, "blank-author", "")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, db.ErrLabelInvalid)

	bug, labeledEvent, err := store.AddLabelAndEvent(ctx, fixture.Issue.ID, db.LabelEventParams{
		EventType: "issue.labeled",
		Label:     "bug",
		Actor:     "label-author",
	})
	if err != nil {
		return fmt.Errorf("add label and event: %w", err)
	}
	assert.Equal(t, "bug", bug.Label)
	assert.Equal(t, "issue.labeled", labeledEvent.Type)
	assert.Equal(t, "label-author", labeledEvent.Actor)
	var labeledPayload struct {
		IssueUID string `json:"issue_uid"`
		Label    string `json:"label"`
	}
	require.NoError(t, json.Unmarshal([]byte(labeledEvent.Payload), &labeledPayload))
	assert.Equal(t, fixture.Issue.UID, labeledPayload.IssueUID)
	assert.Equal(t, "bug", labeledPayload.Label)

	hasBug, err := store.HasLabel(ctx, fixture.Issue.ID, "bug")
	if err != nil {
		return fmt.Errorf("check attached label: %w", err)
	}
	assert.True(t, hasBug)
	hasMissing, err := store.HasLabel(ctx, fixture.Issue.ID, "missing")
	if err != nil {
		return fmt.Errorf("check absent label: %w", err)
	}
	assert.False(t, hasMissing)
	byEndpoints, err := store.LabelByEndpoints(ctx, fixture.Issue.ID, "bug")
	if err != nil {
		return fmt.Errorf("label by endpoints: %w", err)
	}
	assert.Equal(t, bug, byEndpoints)
	_, err = store.LabelByEndpoints(ctx, fixture.Issue.ID, "missing")
	assert.ErrorIs(t, err, db.ErrNotFound)

	rows, err := store.LabelsByIssue(ctx, fixture.Issue.ID)
	if err != nil {
		return fmt.Errorf("labels by issue: %w", err)
	}
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"bug", "priority:high"}, []string{rows[0].Label, rows[1].Label})
	values, err := store.LabelsForIssue(ctx, fixture.Issue.ID)
	if err != nil {
		return fmt.Errorf("label values by issue: %w", err)
	}
	assert.Equal(t, []string{"bug", "priority:high"}, values)

	_, err = store.AddLabel(ctx, second.ID, "bug", "label-author")
	if err != nil {
		return fmt.Errorf("add second issue label: %w", err)
	}
	counts, err := store.LabelCounts(ctx, fixture.Project.ID)
	if err != nil {
		return fmt.Errorf("label counts: %w", err)
	}
	assert.Equal(t, []db.LabelCount{{Label: "bug", Count: 2}, {Label: "priority:high", Count: 1}}, counts)

	other, err := createIssueFixture(ctx, store, "outside-label-project", "outside issue", "label-author", nil)
	if err != nil {
		return err
	}
	_, err = store.AddLabel(ctx, other.Issue.ID, "outside", "label-author")
	if err != nil {
		return fmt.Errorf("add out-of-scope label: %w", err)
	}
	byIssues, err := store.LabelsByIssues(ctx, fixture.Project.ID, []int64{
		fixture.Issue.ID, second.ID, other.Issue.ID,
	})
	if err != nil {
		return fmt.Errorf("labels by issues: %w", err)
	}
	assert.Equal(t, map[int64][]string{
		fixture.Issue.ID: {"bug", "priority:high"},
		second.ID:        {"bug"},
	}, byIssues)
	empty, err := store.LabelsByIssues(ctx, fixture.Project.ID, nil)
	if err != nil {
		return fmt.Errorf("labels by empty issue set: %w", err)
	}
	assert.Empty(t, empty)

	_, _, _, err = store.SoftDeleteIssue(ctx, second.ID, "label-author")
	if err != nil {
		return fmt.Errorf("soft delete labeled issue: %w", err)
	}
	counts, err = store.LabelCounts(ctx, fixture.Project.ID)
	if err != nil {
		return fmt.Errorf("label counts after deletion: %w", err)
	}
	assert.Equal(t, []db.LabelCount{{Label: "bug", Count: 1}, {Label: "priority:high", Count: 1}}, counts)

	if err := store.RemoveLabel(ctx, fixture.Issue.ID, "priority:high"); err != nil {
		return fmt.Errorf("remove direct label: %w", err)
	}
	err = store.RemoveLabel(ctx, fixture.Issue.ID, "priority:high")
	assert.ErrorIs(t, err, db.ErrNotFound)
	unlabeledEvent, err := store.RemoveLabelAndEvent(ctx, fixture.Issue.ID, db.LabelEventParams{
		EventType: "issue.unlabeled",
		Label:     "bug",
		Actor:     "label-author",
	})
	if err != nil {
		return fmt.Errorf("remove label and event: %w", err)
	}
	assert.Equal(t, "issue.unlabeled", unlabeledEvent.Type)
	var unlabeledPayload struct {
		IssueUID string `json:"issue_uid"`
		Label    string `json:"label"`
	}
	require.NoError(t, json.Unmarshal([]byte(unlabeledEvent.Payload), &unlabeledPayload))
	assert.Equal(t, fixture.Issue.UID, unlabeledPayload.IssueUID)
	assert.Equal(t, "bug", unlabeledPayload.Label)
	_, err = store.RemoveLabelAndEvent(ctx, fixture.Issue.ID, db.LabelEventParams{
		EventType: "issue.unlabeled", Label: "bug", Actor: "label-author",
	})
	assert.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkLinksAndRelationshipProjections(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	primary, err := createIssueFixture(ctx, store, "relationship-project", "open child", "link-author", nil)
	if err != nil {
		return err
	}
	closedChild, err := createFixtureIssue(ctx, store, primary.Project.ID, "closed child", "link-author", nil)
	if err != nil {
		return err
	}
	blocker, err := createFixtureIssue(ctx, store, primary.Project.ID, "blocker", "link-author", nil)
	if err != nil {
		return err
	}
	relatedA, err := createFixtureIssue(ctx, store, primary.Project.ID, "related a", "link-author", nil)
	if err != nil {
		return err
	}
	eventSource, err := createFixtureIssue(ctx, store, primary.Project.ID, "event source", "link-author", nil)
	if err != nil {
		return err
	}
	alternateParent, err := createFixtureIssue(ctx, store, primary.Project.ID, "alternate parent", "link-author", nil)
	if err != nil {
		return err
	}

	other, err := createIssueFixture(ctx, store, "peer-relationship-project", "cross-project parent", "link-author", nil)
	if err != nil {
		return err
	}
	blocked, err := createFixtureIssue(ctx, store, other.Project.ID, "blocked issue", "link-author", nil)
	if err != nil {
		return err
	}
	relatedB, err := createFixtureIssue(ctx, store, other.Project.ID, "related b", "link-author", nil)
	if err != nil {
		return err
	}
	eventTarget, err := createFixtureIssue(ctx, store, other.Project.ID, "event target", "link-author", nil)
	if err != nil {
		return err
	}

	parentLink, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: primary.Issue.ID,
		ToIssueID:   other.Issue.ID,
		Type:        "parent",
		Author:      "link-author",
	})
	if err != nil {
		return fmt.Errorf("create cross-project parent link: %w", err)
	}
	assert.NotZero(t, parentLink.ID)
	assert.Equal(t, primary.Issue.UID, parentLink.FromIssueUID)
	assert.Equal(t, other.Issue.UID, parentLink.ToIssueUID)
	assert.Equal(t, "parent", parentLink.Type)
	assert.False(t, parentLink.CreatedAt.IsZero())
	closedParentLink, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: closedChild.ID, ToIssueID: other.Issue.ID, Type: "parent", Author: "link-author",
	})
	if err != nil {
		return fmt.Errorf("create second child link: %w", err)
	}

	byID, err := store.LinkByID(ctx, parentLink.ID)
	if err != nil {
		return fmt.Errorf("link by ID: %w", err)
	}
	byEndpoints, err := store.LinkByEndpoints(ctx, primary.Issue.ID, other.Issue.ID, "parent")
	if err != nil {
		return fmt.Errorf("link by endpoints: %w", err)
	}
	assert.Equal(t, parentLink, byID)
	assert.Equal(t, parentLink, byEndpoints)
	_, err = store.LinkByID(ctx, parentLink.ID+100000)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.LinkByEndpoints(ctx, primary.Issue.ID, other.Issue.ID, "blocks")
	assert.ErrorIs(t, err, db.ErrNotFound)

	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: primary.Issue.ID, ToIssueID: other.Issue.ID, Type: "parent", Author: "link-author",
	})
	assert.ErrorIs(t, err, db.ErrLinkExists)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: primary.Issue.ID, ToIssueID: alternateParent.ID, Type: "parent", Author: "link-author",
	})
	assert.ErrorIs(t, err, db.ErrParentAlreadySet)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: primary.Issue.ID, ToIssueID: primary.Issue.ID, Type: "related", Author: "link-author",
	})
	assert.ErrorIs(t, err, db.ErrSelfLink)

	parent, err := store.ParentOf(ctx, primary.Issue.ID)
	if err != nil {
		return fmt.Errorf("parent of child: %w", err)
	}
	assert.Equal(t, other.Issue.ID, parent.ToIssueID)
	parentNumbers, err := store.ParentNumbersByIssues(ctx, []int64{primary.Issue.ID, closedChild.ID, blocker.ID})
	if err != nil {
		return fmt.Errorf("parent numbers: %w", err)
	}
	assert.Equal(t, map[int64]int64{
		primary.Issue.ID: other.Issue.ID,
		closedChild.ID:   other.Issue.ID,
	}, parentNumbers)
	parentShortIDs, err := store.ParentShortIDsByIssues(ctx, []int64{primary.Issue.ID, closedChild.ID})
	if err != nil {
		return fmt.Errorf("parent short IDs: %w", err)
	}
	assert.Equal(t, map[int64]string{
		primary.Issue.ID: other.Issue.ShortID,
		closedChild.ID:   other.Issue.ShortID,
	}, parentShortIDs)

	_, _, _, err = store.CloseIssue(ctx, closedChild.ID, "done", "link-author", "", nil)
	if err != nil {
		return fmt.Errorf("close child: %w", err)
	}
	childCounts, err := store.ChildCountsByParents(ctx, []int64{other.Issue.ID})
	if err != nil {
		return fmt.Errorf("child counts: %w", err)
	}
	assert.Equal(t, db.ChildCounts{Open: 1, Total: 2}, childCounts[other.Issue.ID])
	children, err := store.ChildrenOfIssue(ctx, other.Issue.ID)
	if err != nil {
		return fmt.Errorf("children of issue: %w", err)
	}
	require.Len(t, children, 2)
	assert.Equal(t, []int64{closedChild.ID, primary.Issue.ID}, []int64{children[0].ID, children[1].ID})
	openChildren, totalOpen, err := store.OpenChildrenOf(ctx, other.Issue.ID, 1)
	if err != nil {
		return fmt.Errorf("open children: %w", err)
	}
	assert.Equal(t, 1, totalOpen)
	require.Len(t, openChildren, 1)
	assert.Equal(t, primary.Issue.ID, openChildren[0].ID)
	parentLinks, err := store.LinksByIssue(ctx, other.Issue.ID)
	if err != nil {
		return fmt.Errorf("links by issue: %w", err)
	}
	require.Len(t, parentLinks, 2)

	blockLink, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: blocker.ID, ToIssueID: blocked.ID, Type: "blocks", Author: "link-author",
	})
	if err != nil {
		return fmt.Errorf("create blocks link: %w", err)
	}
	assert.NotZero(t, blockLink.ID)
	blocks, err := store.BlockNumbersByIssues(ctx, []int64{blocker.ID})
	if err != nil {
		return fmt.Errorf("block numbers: %w", err)
	}
	assert.Equal(t, map[int64][]int64{blocker.ID: {blocked.ID}}, blocks)
	blockedBy, err := store.BlockedByNumbersByIssues(ctx, []int64{blocked.ID})
	if err != nil {
		return fmt.Errorf("blocked-by numbers: %w", err)
	}
	assert.Equal(t, map[int64][]int64{blocked.ID: {blocker.ID}}, blockedBy)
	activelyBlocked, err := store.ActivelyBlockedIssueIDs(ctx, []int64{blocked.ID})
	if err != nil {
		return fmt.Errorf("active blocked projection: %w", err)
	}
	assert.Equal(t, map[int64]bool{blocked.ID: true}, activelyBlocked)
	_, _, _, err = store.CloseIssue(ctx, blocker.ID, "done", "link-author", "", nil)
	if err != nil {
		return fmt.Errorf("close blocker: %w", err)
	}
	activelyBlocked, err = store.ActivelyBlockedIssueIDs(ctx, []int64{blocked.ID})
	if err != nil {
		return fmt.Errorf("active blocked projection after close: %w", err)
	}
	assert.Empty(t, activelyBlocked)

	relatedLink, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: relatedA.ID, ToIssueID: relatedB.ID, Type: "related", Author: "link-author",
	})
	if err != nil {
		return fmt.Errorf("create related link: %w", err)
	}
	assert.NotZero(t, relatedLink.ID)
	related, err := store.RelatedNumbersByIssues(ctx, []int64{relatedA.ID, relatedB.ID})
	if err != nil {
		return fmt.Errorf("related numbers: %w", err)
	}
	assert.Equal(t, map[int64][]int64{
		relatedA.ID: {relatedB.ID},
		relatedB.ID: {relatedA.ID},
	}, related)

	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: other.Issue.ID,
		ToIssueID:   primary.Issue.ID,
		Type:        "parent",
		Author:      "link-author",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: other.Issue.ID,
		FromShortID:  other.Issue.ShortID,
		FromUID:      other.Issue.UID,
		ToShortID:    primary.Issue.ShortID,
		ToUID:        primary.Issue.UID,
		Actor:        "link-author",
	})
	assert.ErrorIs(t, err, db.ErrParentCycle)

	eventLink, linkedEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: eventSource.ID,
		ToIssueID:   eventTarget.ID,
		Type:        "related",
		Author:      "link-author",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: eventSource.ID,
		FromShortID:  eventSource.ShortID,
		FromUID:      eventSource.UID,
		ToShortID:    eventTarget.ShortID,
		ToUID:        eventTarget.UID,
	})
	if err != nil {
		return fmt.Errorf("create link and event: %w", err)
	}
	assert.Equal(t, "link-author", eventLink.Author)
	assert.Equal(t, "issue.linked", linkedEvent.Type)
	assert.Equal(t, "link-author", linkedEvent.Actor)
	require.NotNil(t, linkedEvent.RelatedIssueID)
	assert.Equal(t, eventTarget.ID, *linkedEvent.RelatedIssueID)
	var linkedPayload struct {
		LinkID  int64  `json:"link_id"`
		Type    string `json:"type"`
		FromUID string `json:"from_uid"`
		ToUID   string `json:"to_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(linkedEvent.Payload), &linkedPayload))
	assert.Equal(t, eventLink.ID, linkedPayload.LinkID)
	assert.Equal(t, "related", linkedPayload.Type)
	assert.Equal(t, eventSource.UID, linkedPayload.FromUID)
	assert.Equal(t, eventTarget.UID, linkedPayload.ToUID)

	unlinkedEvent, err := store.DeleteLinkAndEvent(ctx, eventLink, db.LinkEventParams{
		EventType:    "issue.unlinked",
		EventIssueID: eventSource.ID,
		FromShortID:  eventSource.ShortID,
		FromUID:      eventSource.UID,
		ToShortID:    eventTarget.ShortID,
		ToUID:        eventTarget.UID,
		Actor:        "unlink-author",
	})
	if err != nil {
		return fmt.Errorf("delete link and event: %w", err)
	}
	assert.Equal(t, "issue.unlinked", unlinkedEvent.Type)
	assert.Equal(t, "unlink-author", unlinkedEvent.Actor)
	_, err = store.LinkByID(ctx, eventLink.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.DeleteLinkAndEvent(ctx, eventLink, db.LinkEventParams{
		EventType: "issue.unlinked", EventIssueID: eventSource.ID, Actor: "unlink-author",
	})
	assert.ErrorIs(t, err, db.ErrNotFound)

	if err := store.DeleteLinkByID(ctx, closedParentLink.ID); err != nil {
		return fmt.Errorf("delete link by ID: %w", err)
	}
	err = store.DeleteLinkByID(ctx, closedParentLink.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.ParentOf(ctx, closedChild.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)

	emptyParents, err := store.ParentNumbersByIssues(ctx, nil)
	if err != nil {
		return fmt.Errorf("empty parent projection: %w", err)
	}
	assert.Empty(t, emptyParents)
	return nil
}

func checkReadyQueuesAndDiscovery(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	alice := "alice"
	primary, err := createIssueFixture(
		ctx, store, "discovery-project", "login crash", "discovery-author", &alice,
	)
	if err != nil {
		return err
	}
	newBody := "browser failure"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{
		IssueID: primary.Issue.ID, Body: &newBody, Actor: "discovery-author",
	})
	if err != nil {
		return fmt.Errorf("add discovery body: %w", err)
	}
	_, err = store.AddLabel(ctx, primary.Issue.ID, "bug", "discovery-author")
	if err != nil {
		return fmt.Errorf("add bug label: %w", err)
	}
	_, err = store.AddLabel(ctx, primary.Issue.ID, "p0", "discovery-author")
	if err != nil {
		return fmt.Errorf("add priority label: %w", err)
	}
	blocker, err := createFixtureIssue(ctx, store, primary.Project.ID, "open dependency", "discovery-author", nil)
	if err != nil {
		return err
	}
	blocked, err := createFixtureIssue(ctx, store, primary.Project.ID, "login blocked", "discovery-author", nil)
	if err != nil {
		return err
	}
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: blocker.ID, ToIssueID: blocked.ID, Type: "blocks", Author: "discovery-author",
	})
	if err != nil {
		return fmt.Errorf("create readiness blocker: %w", err)
	}
	closed, err := createFixtureIssue(ctx, store, primary.Project.ID, "login closed", "discovery-author", nil)
	if err != nil {
		return err
	}
	_, _, _, err = store.CloseIssue(ctx, closed.ID, "done", "discovery-author", "", nil)
	if err != nil {
		return fmt.Errorf("close discovery issue: %w", err)
	}
	deleted, err := createFixtureIssue(ctx, store, primary.Project.ID, "deletedtoken", "discovery-author", nil)
	if err != nil {
		return err
	}
	_, _, _, err = store.SoftDeleteIssue(ctx, deleted.ID, "discovery-author")
	if err != nil {
		return fmt.Errorf("delete discovery issue: %w", err)
	}
	other, err := createIssueFixture(ctx, store, "other-discovery-project", "login elsewhere", "discovery-author", nil)
	if err != nil {
		return err
	}

	ready, err := store.ReadyIssues(ctx, primary.Project.ID, 0, db.ReadyIssuesFilter{})
	if err != nil {
		return fmt.Errorf("ready issues: %w", err)
	}
	assert.Contains(t, issueIDs(ready), primary.Issue.ID)
	assert.Contains(t, issueIDs(ready), blocker.ID)
	assert.NotContains(t, issueIDs(ready), blocked.ID)
	assert.NotContains(t, issueIDs(ready), closed.ID)
	assert.NotContains(t, issueIDs(ready), deleted.ID)
	owned, err := store.ReadyIssues(ctx, primary.Project.ID, 0, db.ReadyIssuesFilter{Owner: "alice"})
	if err != nil {
		return fmt.Errorf("ready issues by owner: %w", err)
	}
	require.Len(t, owned, 1)
	assert.Equal(t, primary.Issue.ID, owned[0].ID)
	unowned, err := store.ReadyIssues(ctx, primary.Project.ID, 0, db.ReadyIssuesFilter{Unowned: true})
	if err != nil {
		return fmt.Errorf("ready unowned issues: %w", err)
	}
	assert.NotContains(t, issueIDs(unowned), primary.Issue.ID)
	withLabels, err := store.ReadyIssues(ctx, primary.Project.ID, 0, db.ReadyIssuesFilter{
		Labels: []string{"BUG", "p0"},
	})
	if err != nil {
		return fmt.Errorf("ready issues by labels: %w", err)
	}
	require.Len(t, withLabels, 1)
	assert.Equal(t, primary.Issue.ID, withLabels[0].ID)
	withoutBug, err := store.ReadyIssues(ctx, primary.Project.ID, 0, db.ReadyIssuesFilter{
		ExcludeLabels: []string{"BUG"},
	})
	if err != nil {
		return fmt.Errorf("ready issues excluding labels: %w", err)
	}
	assert.NotContains(t, issueIDs(withoutBug), primary.Issue.ID)
	limited, err := store.ReadyIssues(ctx, primary.Project.ID, 1, db.ReadyIssuesFilter{})
	if err != nil {
		return fmt.Errorf("limited ready issues: %w", err)
	}
	require.Len(t, limited, 1)

	global, err := store.ReadyIssuesGlobal(ctx, 0)
	if err != nil {
		return fmt.Errorf("global ready issues: %w", err)
	}
	globalIDs := readyGlobalIssueIDs(global)
	assert.Contains(t, globalIDs, primary.Issue.ID)
	assert.Contains(t, globalIDs, other.Issue.ID)
	assert.NotContains(t, globalIDs, blocked.ID)
	for _, row := range global {
		if row.ID == other.Issue.ID {
			assert.Equal(t, other.Project.Name, row.ProjectName)
		}
	}
	_, _, _, err = store.CloseIssue(ctx, blocker.ID, "done", "discovery-author", "", nil)
	if err != nil {
		return fmt.Errorf("close readiness blocker: %w", err)
	}
	ready, err = store.ReadyIssues(ctx, primary.Project.ID, 0, db.ReadyIssuesFilter{})
	if err != nil {
		return fmt.Errorf("ready issues after blocker close: %w", err)
	}
	assert.Contains(t, issueIDs(ready), blocked.ID)

	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: primary.Issue.ID, Author: "discovery-author", Body: "watermelon note",
	})
	if err != nil {
		return fmt.Errorf("create searchable comment: %w", err)
	}
	assert.NotZero(t, comment.ID)
	andMatches, err := store.SearchFTS(ctx, primary.Project.ID, "login browser", 20, false)
	if err != nil {
		return fmt.Errorf("search all terms: %w", err)
	}
	require.Len(t, andMatches, 1)
	assert.Equal(t, primary.Issue.ID, andMatches[0].Issue.ID)
	assert.Equal(t, []string{"title", "body"}, andMatches[0].MatchedIn)
	commentMatches, err := store.SearchFTS(ctx, primary.Project.ID, "watermelon", 20, false)
	if err != nil {
		return fmt.Errorf("search comments: %w", err)
	}
	require.Len(t, commentMatches, 1)
	assert.Equal(t, []string{"comments"}, commentMatches[0].MatchedIn)
	anyMatches, err := store.SearchFTSAny(ctx, primary.Project.ID, "login absenttoken", 20, false)
	if err != nil {
		return fmt.Errorf("search any term: %w", err)
	}
	assert.Contains(t, searchCandidateIDs(anyMatches), primary.Issue.ID)
	assert.Contains(t, searchCandidateIDs(anyMatches), blocked.ID)
	sparse, err := createFixtureIssue(ctx, store, primary.Project.ID, "ranktoken once", "discovery-author", nil)
	if err != nil {
		return err
	}
	dense, err := createFixtureIssue(ctx, store, primary.Project.ID,
		"ranktoken ranktoken ranktoken", "discovery-author", nil)
	if err != nil {
		return err
	}
	ranked, err := store.SearchFTS(ctx, primary.Project.ID, "ranktoken", 20, false)
	if err != nil {
		return fmt.Errorf("search relevance ranking: %w", err)
	}
	require.Len(t, ranked, 2)
	assert.Equal(t, dense.ID, ranked[0].Issue.ID)
	assert.Equal(t, sparse.ID, ranked[1].Issue.ID)
	assert.GreaterOrEqual(t, ranked[0].Score, ranked[1].Score)
	deletedMatches, err := store.SearchFTS(ctx, primary.Project.ID, "deletedtoken", 20, false)
	if err != nil {
		return fmt.Errorf("search excluding deleted: %w", err)
	}
	assert.Empty(t, deletedMatches)
	deletedMatches, err = store.SearchFTS(ctx, primary.Project.ID, "deletedtoken", 20, true)
	if err != nil {
		return fmt.Errorf("search including deleted: %w", err)
	}
	require.Len(t, deletedMatches, 1)
	assert.Equal(t, deleted.ID, deletedMatches[0].Issue.ID)
	blankMatches, err := store.SearchFTS(ctx, primary.Project.ID, "   ", 20, false)
	if err != nil {
		return fmt.Errorf("blank search: %w", err)
	}
	assert.Empty(t, blankMatches)

	qualifiers, err := store.IssueQualifiersByUIDs(ctx, []string{
		primary.Issue.UID, other.Issue.UID, "01ARZ3NDEKTSV4RRFFQ69G5FAV",
	})
	if err != nil {
		return fmt.Errorf("issue qualifiers: %w", err)
	}
	assert.Equal(t, db.IssueQualifier{
		ProjectID: primary.Project.ID, ProjectName: primary.Project.Name, ShortID: primary.Issue.ShortID,
	}, qualifiers[primary.Issue.UID])
	assert.Equal(t, db.IssueQualifier{
		ProjectID: other.Project.ID, ProjectName: other.Project.Name, ShortID: other.Issue.ShortID,
	}, qualifiers[other.Issue.UID])
	assert.NotContains(t, qualifiers, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	emptyQualifiers, err := store.IssueQualifiersByUIDs(ctx, nil)
	if err != nil {
		return fmt.Errorf("empty issue qualifiers: %w", err)
	}
	assert.Empty(t, emptyQualifiers)

	firstPage, err := store.ListIssueContent(ctx, 0, 2)
	if err != nil {
		return fmt.Errorf("first issue content page: %w", err)
	}
	require.Len(t, firstPage, 2)
	assert.Less(t, firstPage[0].ID, firstPage[1].ID)
	secondPage, err := store.ListIssueContent(ctx, firstPage[1].ID, 100)
	if err != nil {
		return fmt.Errorf("second issue content page: %w", err)
	}
	for _, content := range append(firstPage, secondPage...) {
		assert.NotEqual(t, deleted.ID, content.ID)
		assert.NotEmpty(t, content.ProjectUID)
	}
	return nil
}
