package sqlitestore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCreateLink_RoundTrips(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createKataProject(ctx, t, d)
	a := makeIssue(t, ctx, d, p.ID, "child", "tester")
	b := makeIssue(t, ctx, d, p.ID, "parent", "tester")

	link := makeLink(ctx, t, d, p.ID, a.ID, b.ID, "parent")
	assert.Greater(t, link.ID, int64(0))
	assert.Equal(t, "parent", link.Type)
	assert.Equal(t, a.ID, link.FromIssueID)
	assert.Equal(t, b.ID, link.ToIssueID)
	assert.Equal(t, a.UID, link.FromIssueUID)
	assert.Equal(t, b.UID, link.ToIssueUID)

	got, err := d.LinkByID(ctx, link.ID)
	require.NoError(t, err)
	assert.Equal(t, link.ID, got.ID)
	assert.Equal(t, a.UID, got.FromIssueUID)
	assert.Equal(t, b.UID, got.ToIssueUID)
}

func TestLinksRejectMismatchedUIDCache(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")

	_, err := d.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, ?, ?, 'blocks', 'tester')`,
		p.ID, a.ID, b.ID, b.UID, b.UID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "from_issue_uid does not match from_issue_id")

	link := makeLink(ctx, t, d, p.ID, a.ID, b.ID, "blocks")
	_, err = d.ExecContext(ctx,
		`UPDATE links SET to_issue_uid = ? WHERE id = ?`, a.UID, link.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "to_issue_uid does not match to_issue_id")
}

func TestCreateLink_DuplicateIsErrLinkExists(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createKataProject(ctx, t, d)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")

	makeLink(ctx, t, d, p.ID, a.ID, b.ID, "blocks")
	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		ProjectID: p.ID, FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks", Author: "tester",
	})
	assert.True(t, errors.Is(err, db.ErrLinkExists), "expected ErrLinkExists, got %v", err)
}

func TestCreateLink_SecondParentIsErrParentAlreadySet(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createKataProject(ctx, t, d)
	child := makeIssue(t, ctx, d, p.ID, "child", "tester")
	p1 := makeIssue(t, ctx, d, p.ID, "p1", "tester")
	p2 := makeIssue(t, ctx, d, p.ID, "p2", "tester")

	makeLink(ctx, t, d, p.ID, child.ID, p1.ID, "parent")
	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		ProjectID: p.ID, FromIssueID: child.ID, ToIssueID: p2.ID, Type: "parent", Author: "tester",
	})
	assert.True(t, errors.Is(err, db.ErrParentAlreadySet),
		"expected ErrParentAlreadySet, got %v", err)
}

func TestCreateLink_ExactDuplicateParentIsErrLinkExists(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	child := makeIssue(t, ctx, d, p.ID, "child", "tester")
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")

	// First insert succeeds.
	makeLink(ctx, t, d, p.ID, child.ID, parent.ID, "parent")

	// Re-inserting the exact same triple is "already linked" (idempotent
	// no-op), not "different parent set". Must be ErrLinkExists.
	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		ProjectID: p.ID, FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "tester",
	})
	assert.True(t, errors.Is(err, db.ErrLinkExists),
		"exact duplicate parent must be ErrLinkExists, got %v", err)
}

func TestCreateLink_CrossProjectIsErrCrossProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p1 := createProject(ctx, t, d, "p1")
	p2 := createProject(ctx, t, d, "p2")
	a := makeIssue(t, ctx, d, p1.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p2.ID, "b", "tester")

	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		ProjectID: p1.ID, FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks", Author: "tester",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrCrossProjectLink),
		"expected ErrCrossProjectLink, got %v", err)
}

func TestCreateLink_SelfLinkIsError(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")

	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		ProjectID: p.ID, FromIssueID: a.ID, ToIssueID: a.ID, Type: "related", Author: "tester",
	})
	assert.True(t, errors.Is(err, db.ErrSelfLink),
		"expected ErrSelfLink, got %v", err)
}

func TestLinkByEndpoints_FindsExisting(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")
	created := makeLink(ctx, t, d, p.ID, a.ID, b.ID, "related")

	got, err := d.LinkByEndpoints(ctx, a.ID, b.ID, "related")
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)

	_, err = d.LinkByEndpoints(ctx, a.ID, b.ID, "parent")
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestLinksByIssue_ReturnsBothDirections(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")
	c := makeIssue(t, ctx, d, p.ID, "c", "tester")
	// a → blocks → b ; c → parent → a
	makeLink(ctx, t, d, p.ID, a.ID, b.ID, "blocks")
	makeLink(ctx, t, d, p.ID, c.ID, a.ID, "parent")

	got, err := d.LinksByIssue(ctx, a.ID)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestParentOf_ReturnsErrNotFoundWhenAbsent(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")

	_, err := d.ParentOf(ctx, a.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestParentNumbersByIssues_EmptyInput(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	got, err := d.ParentNumbersByIssues(ctx, p.ID, nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	got, err = d.ParentNumbersByIssues(ctx, p.ID, []int64{})
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestChildCountsByParents_EmptyInput(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	got, err := d.ChildCountsByParents(ctx, p.ID, nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	got, err = d.ChildCountsByParents(ctx, p.ID, []int64{})
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestParentNumbersByIssues_ReturnsImmediateParents(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")
	child1 := makeIssue(t, ctx, d, p.ID, "child 1", "tester")
	child2 := makeIssue(t, ctx, d, p.ID, "child 2", "tester")
	unrelated := makeIssue(t, ctx, d, p.ID, "unrelated", "tester")

	makeLink(ctx, t, d, p.ID, child1.ID, parent.ID, "parent")
	makeLink(ctx, t, d, p.ID, child2.ID, parent.ID, "parent")

	got, err := d.ParentNumbersByIssues(ctx, p.ID, []int64{child1.ID, child2.ID, unrelated.ID})
	require.NoError(t, err)
	assert.Equal(t, parent.ID, got[child1.ID])
	assert.Equal(t, parent.ID, got[child2.ID])
	assert.NotContains(t, got, unrelated.ID)
}

func TestParentNumbersByIssues_ConstrainsProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	pa := createProject(ctx, t, d, "a")
	pb := createProject(ctx, t, d, "b")
	parentA := makeIssue(t, ctx, d, pa.ID, "parent a", "tester")
	childA := makeIssue(t, ctx, d, pa.ID, "child a", "tester")
	parentB := makeIssue(t, ctx, d, pb.ID, "parent b", "tester")
	childB := makeIssue(t, ctx, d, pb.ID, "child b", "tester")

	makeLink(ctx, t, d, pa.ID, childA.ID, parentA.ID, "parent")
	makeLink(ctx, t, d, pb.ID, childB.ID, parentB.ID, "parent")

	got, err := d.ParentNumbersByIssues(ctx, pa.ID, []int64{childA.ID, childB.ID})
	require.NoError(t, err)
	assert.Equal(t, parentA.ID, got[childA.ID])
	assert.NotContains(t, got, childB.ID)
}

func TestChildCountsByParents_ReturnsOpenAndTotalDirectChildren(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")
	child1 := makeIssue(t, ctx, d, p.ID, "child 1", "tester")
	child2 := makeIssue(t, ctx, d, p.ID, "child 2", "tester")
	child3 := makeIssue(t, ctx, d, p.ID, "child 3", "tester")
	for _, child := range []db.Issue{child1, child2, child3} {
		makeLink(ctx, t, d, p.ID, child.ID, parent.ID, "parent")
	}
	_, _, _, err := d.CloseIssue(ctx, child2.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	got, err := d.ChildCountsByParents(ctx, p.ID, []int64{parent.ID})
	require.NoError(t, err)
	assert.Equal(t, db.ChildCounts{Open: 2, Total: 3}, got[parent.ID])
}

func TestChildrenOfIssue_ReturnsDirectChildrenOnly(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")
	child1 := makeIssue(t, ctx, d, p.ID, "child 1", "tester")
	child2 := makeIssue(t, ctx, d, p.ID, "child 2", "tester")
	grandchild := makeIssue(t, ctx, d, p.ID, "grandchild", "tester")
	makeLink(ctx, t, d, p.ID, child1.ID, parent.ID, "parent")
	makeLink(ctx, t, d, p.ID, child2.ID, parent.ID, "parent")
	makeLink(ctx, t, d, p.ID, grandchild.ID, child1.ID, "parent")

	got, err := d.ChildrenOfIssue(ctx, p.ID, parent.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, child2.ID, got[0].ID)
	assert.Equal(t, child1.ID, got[1].ID)
}

func TestChildCountsByParents_ChunksLargeInputs(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	const parentCount = 501
	parentIDs := make([]int64, 0, parentCount)
	for i := 0; i < parentCount; i++ {
		parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")
		parentIDs = append(parentIDs, parent.ID)
	}
	child := makeIssue(t, ctx, d, p.ID, "child", "tester")
	makeLink(ctx, t, d, p.ID, child.ID, parentIDs[parentCount-1], "parent")

	got, err := d.ChildCountsByParents(ctx, p.ID, parentIDs)
	require.NoError(t, err, "large parent batches must be chunked under SQLite parameter limits")
	assert.Equal(t, db.ChildCounts{Open: 1, Total: 1}, got[parentIDs[parentCount-1]])
	assert.NotContains(t, got, parentIDs[0])
}

func TestDeleteLinkByID_RemovesRow(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")
	link := makeLink(ctx, t, d, p.ID, a.ID, b.ID, "blocks")

	require.NoError(t, d.DeleteLinkByID(ctx, link.ID))
	_, err := d.LinkByID(ctx, link.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound))

	// Re-deleting returns ErrNotFound (caller decides whether to surface as
	// no-op or 404).
	err = d.DeleteLinkByID(ctx, link.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}
