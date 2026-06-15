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

	link := makeLink(ctx, t, d, a.ID, b.ID, "parent")
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
		`INSERT INTO links(from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, ?, 'blocks', 'tester')`,
		a.ID, b.ID, b.UID, b.UID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "from_issue_uid does not match from_issue_id")

	link := makeLink(ctx, t, d, a.ID, b.ID, "blocks")
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

	makeLink(ctx, t, d, a.ID, b.ID, "blocks")
	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks", Author: "tester",
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

	makeLink(ctx, t, d, child.ID, p1.ID, "parent")
	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: p2.ID, Type: "parent", Author: "tester",
	})
	assert.True(t, errors.Is(err, db.ErrParentAlreadySet),
		"expected ErrParentAlreadySet, got %v", err)
}

func TestCreateLink_ExactDuplicateParentIsErrLinkExists(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	child := makeIssue(t, ctx, d, p.ID, "child", "tester")
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")

	// First insert succeeds.
	makeLink(ctx, t, d, child.ID, parent.ID, "parent")

	// Re-inserting the exact same triple is "already linked" (idempotent
	// no-op), not "different parent set". Must be ErrLinkExists.
	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "tester",
	})
	assert.True(t, errors.Is(err, db.ErrLinkExists),
		"exact duplicate parent must be ErrLinkExists, got %v", err)
}

// TestCreateLink_CrossProject pins storage v16: links are project-independent
// edges, so endpoints in different projects are legal at the db layer.
func TestCreateLink_CrossProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	pa := createProject(ctx, t, d, "alpha")
	pb := createProject(ctx, t, d, "beta")
	ia := makeIssue(t, ctx, d, pa.ID, "issue in alpha", "tester")
	ib := makeIssue(t, ctx, d, pb.ID, "issue in beta", "tester")

	link, err := d.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: ia.ID,
		ToIssueID:   ib.ID,
		Type:        "blocks",
		Author:      "tester",
	})
	require.NoError(t, err, "cross-project link must be legal at the store level")
	assert.Equal(t, ia.ID, link.FromIssueID)
	assert.Equal(t, ib.ID, link.ToIssueID)
	assert.Equal(t, ia.UID, link.FromIssueUID, "uid denormalization intact")
	assert.Equal(t, ib.UID, link.ToIssueUID)
}

func TestCreateLink_SelfLinkIsError(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")

	_, err := d.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: a.ID, ToIssueID: a.ID, Type: "related", Author: "tester",
	})
	assert.True(t, errors.Is(err, db.ErrSelfLink),
		"expected ErrSelfLink, got %v", err)
}

func TestLinkByEndpoints_FindsExisting(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")
	created := makeLink(ctx, t, d, a.ID, b.ID, "related")

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
	makeLink(ctx, t, d, a.ID, b.ID, "blocks")
	makeLink(ctx, t, d, c.ID, a.ID, "parent")

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
	d, ctx, _ := setupTestProject(t)

	got, err := d.ParentNumbersByIssues(ctx, nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	got, err = d.ParentNumbersByIssues(ctx, []int64{})
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestChildCountsByParents_EmptyInput(t *testing.T) {
	d, ctx, _ := setupTestProject(t)

	got, err := d.ChildCountsByParents(ctx, nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	got, err = d.ChildCountsByParents(ctx, []int64{})
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

	makeLink(ctx, t, d, child1.ID, parent.ID, "parent")
	makeLink(ctx, t, d, child2.ID, parent.ID, "parent")

	got, err := d.ParentNumbersByIssues(ctx, []int64{child1.ID, child2.ID, unrelated.ID})
	require.NoError(t, err)
	assert.Equal(t, parent.ID, got[child1.ID])
	assert.Equal(t, parent.ID, got[child2.ID])
	assert.NotContains(t, got, unrelated.ID)
}

func TestChildCountsByParents_ReturnsOpenAndTotalDirectChildren(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")
	child1 := makeIssue(t, ctx, d, p.ID, "child 1", "tester")
	child2 := makeIssue(t, ctx, d, p.ID, "child 2", "tester")
	child3 := makeIssue(t, ctx, d, p.ID, "child 3", "tester")
	for _, child := range []db.Issue{child1, child2, child3} {
		makeLink(ctx, t, d, child.ID, parent.ID, "parent")
	}
	_, _, _, err := d.CloseIssue(ctx, child2.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	got, err := d.ChildCountsByParents(ctx, []int64{parent.ID})
	require.NoError(t, err)
	assert.Equal(t, db.ChildCounts{Open: 2, Total: 3}, got[parent.ID])
}

func TestChildrenOfIssue_ReturnsDirectChildrenOnly(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")
	child1 := makeIssue(t, ctx, d, p.ID, "child 1", "tester")
	child2 := makeIssue(t, ctx, d, p.ID, "child 2", "tester")
	grandchild := makeIssue(t, ctx, d, p.ID, "grandchild", "tester")
	makeLink(ctx, t, d, child1.ID, parent.ID, "parent")
	makeLink(ctx, t, d, child2.ID, parent.ID, "parent")
	makeLink(ctx, t, d, grandchild.ID, child1.ID, "parent")

	got, err := d.ChildrenOfIssue(ctx, parent.ID)
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
	makeLink(ctx, t, d, child.ID, parentIDs[parentCount-1], "parent")

	got, err := d.ChildCountsByParents(ctx, parentIDs)
	require.NoError(t, err, "large parent batches must be chunked under SQLite parameter limits")
	assert.Equal(t, db.ChildCounts{Open: 1, Total: 1}, got[parentIDs[parentCount-1]])
	assert.NotContains(t, got, parentIDs[0])
}

func TestDeleteLinkByID_RemovesRow(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")
	link := makeLink(ctx, t, d, a.ID, b.ID, "blocks")

	require.NoError(t, d.DeleteLinkByID(ctx, link.ID))
	_, err := d.LinkByID(ctx, link.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound))

	// Re-deleting returns ErrNotFound (caller decides whether to surface as
	// no-op or 404).
	err = d.DeleteLinkByID(ctx, link.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

// TestRelationshipQueries_CrossProjectPeersVisible pins that every read query
// in queries_links.go traverses edges regardless of endpoint project: storage
// v16 drops project_id from links entirely, so no filter can re-scope them.
//
// Seed layout:
//   - project alpha: child, relA
//   - project beta:  parent, blocked, relB
//   - child  --parent--> parent   (cross-project)
//   - child  --blocks--> blocked  (cross-project)
//   - relA   --related-- relB     (cross-project; relA.ID < relB.ID guaranteed
//     by creation order)
func TestRelationshipQueries_CrossProjectPeersVisible(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	alpha := createProject(ctx, t, d, "alpha")
	beta := createProject(ctx, t, d, "beta")

	// Create in order so relA.ID < relB.ID (SQLite auto-increment).
	child := makeIssue(t, ctx, d, alpha.ID, "child", "tester")
	relA := makeIssue(t, ctx, d, alpha.ID, "relA", "tester")
	parent := makeIssue(t, ctx, d, beta.ID, "parent", "tester")
	blocked := makeIssue(t, ctx, d, beta.ID, "blocked", "tester")
	relB := makeIssue(t, ctx, d, beta.ID, "relB", "tester")

	makeLink(ctx, t, d, child.ID, parent.ID, "parent")
	makeLink(ctx, t, d, child.ID, blocked.ID, "blocks")
	// related canonical ordering: from_issue_id < to_issue_id.
	if relA.ID < relB.ID {
		makeLink(ctx, t, d, relA.ID, relB.ID, "related")
	} else {
		makeLink(ctx, t, d, relB.ID, relA.ID, "related")
	}

	t.Run("ParentShortIDsByIssues", func(t *testing.T) {
		got, err := d.ParentShortIDsByIssues(ctx, []int64{child.ID})
		require.NoError(t, err)
		assert.Equal(t, parent.ShortID, got[child.ID],
			"child's parent short_id must resolve across project boundary")
	})

	t.Run("ParentNumbersByIssues", func(t *testing.T) {
		got, err := d.ParentNumbersByIssues(ctx, []int64{child.ID})
		require.NoError(t, err)
		assert.Equal(t, parent.ID, got[child.ID],
			"child's parent id must resolve across project boundary")
	})

	t.Run("BlockNumbersByIssues", func(t *testing.T) {
		got, err := d.BlockNumbersByIssues(ctx, []int64{child.ID})
		require.NoError(t, err)
		require.Contains(t, got, child.ID,
			"blocker must have an entry in the map")
		assert.Equal(t, []int64{blocked.ID}, got[child.ID],
			"blocked issue id must be visible from the blocker across project boundary")
	})

	t.Run("BlockedByNumbersByIssues", func(t *testing.T) {
		got, err := d.BlockedByNumbersByIssues(ctx, []int64{blocked.ID})
		require.NoError(t, err)
		require.Contains(t, got, blocked.ID,
			"blocked issue must have an entry in the map")
		assert.Equal(t, []int64{child.ID}, got[blocked.ID],
			"blocker id must be visible from the blocked issue across project boundary")
	})

	t.Run("RelatedNumbersByIssues_fromSide", func(t *testing.T) {
		got, err := d.RelatedNumbersByIssues(ctx, []int64{relA.ID})
		require.NoError(t, err)
		require.Contains(t, got, relA.ID,
			"relA must appear in the map")
		assert.Equal(t, []int64{relB.ID}, got[relA.ID],
			"relB id must be visible from relA across project boundary")
	})

	t.Run("RelatedNumbersByIssues_toSide", func(t *testing.T) {
		got, err := d.RelatedNumbersByIssues(ctx, []int64{relB.ID})
		require.NoError(t, err)
		require.Contains(t, got, relB.ID,
			"relB must appear in the map")
		assert.Equal(t, []int64{relA.ID}, got[relB.ID],
			"relA id must be visible from relB across project boundary (UNION handles canonical direction)")
	})

	t.Run("ChildCountsByParents", func(t *testing.T) {
		got, err := d.ChildCountsByParents(ctx, []int64{parent.ID})
		require.NoError(t, err)
		assert.Equal(t, db.ChildCounts{Open: 1, Total: 1}, got[parent.ID],
			"alpha-child must be counted under beta-parent across project boundary")
	})

	t.Run("OpenChildrenOf", func(t *testing.T) {
		children, total, err := d.OpenChildrenOf(ctx, parent.ID, 10)
		require.NoError(t, err)
		assert.Equal(t, 1, total,
			"open-children count must include alpha-child across project boundary")
		require.Len(t, children, 1)
		assert.Equal(t, child.ID, children[0].ID,
			"returned child must be the alpha-child")
	})

	t.Run("ChildrenOfIssue", func(t *testing.T) {
		children, err := d.ChildrenOfIssue(ctx, parent.ID)
		require.NoError(t, err)
		require.Len(t, children, 1)
		assert.Equal(t, child.ID, children[0].ID,
			"ChildrenOfIssue must return alpha-child for beta-parent across project boundary")
	})

	t.Run("LinksByIssue", func(t *testing.T) {
		links, err := d.LinksByIssue(ctx, child.ID)
		require.NoError(t, err)
		// child has two links: --parent--> beta-parent and --blocks--> beta-blocked.
		require.Len(t, links, 2,
			"LinksByIssue must return both cross-project edges for alpha-child")
		seen := map[string]bool{}
		for _, l := range links {
			seen[l.Type] = true
		}
		assert.True(t, seen["parent"], "parent edge must appear")
		assert.True(t, seen["blocks"], "blocks edge must appear")
	})
}

// TestOpenChildrenOf_ExcludesArchivedProjectChildren: an open child living in
// an archived project must not surface in the parent-close completeness check
// — neither in the listing nor the count — so an active parent is not blocked
// from closing by a child hidden behind an archived project.
func TestOpenChildrenOf_ExcludesArchivedProjectChildren(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	pa := createProject(ctx, t, d, "alpha")
	pb := createProject(ctx, t, d, "beta")
	parent := makeIssue(t, ctx, d, pa.ID, "parent", "tester")
	liveChild := makeIssue(t, ctx, d, pa.ID, "live child", "tester")
	archivedChild := makeIssue(t, ctx, d, pb.ID, "archived child", "tester")
	makeLink(ctx, t, d, liveChild.ID, parent.ID, "parent")
	makeLink(ctx, t, d, archivedChild.ID, parent.ID, "parent")
	archiveProjectByID(ctx, t, d, pb.ID)

	got, total, err := d.OpenChildrenOf(ctx, parent.ID, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, total, "archived-project child must not count")
	require.Len(t, got, 1)
	assert.Equal(t, liveChild.ID, got[0].ID)
}

// TestChildrenOfIssue_ExcludesArchivedProjectChildren: the surface child
// listing must omit children whose project is archived.
func TestChildrenOfIssue_ExcludesArchivedProjectChildren(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	pa := createProject(ctx, t, d, "alpha")
	pb := createProject(ctx, t, d, "beta")
	parent := makeIssue(t, ctx, d, pa.ID, "parent", "tester")
	liveChild := makeIssue(t, ctx, d, pa.ID, "live child", "tester")
	archivedChild := makeIssue(t, ctx, d, pb.ID, "archived child", "tester")
	makeLink(ctx, t, d, liveChild.ID, parent.ID, "parent")
	makeLink(ctx, t, d, archivedChild.ID, parent.ID, "parent")
	archiveProjectByID(ctx, t, d, pb.ID)

	got, err := d.ChildrenOfIssue(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, got, 1, "archived-project child must not be listed")
	assert.Equal(t, liveChild.ID, got[0].ID)
}

// TestChildCountsByParents_ExcludesArchivedProjectChildren: child counts on
// queue/detail rows must not include children in archived projects.
func TestChildCountsByParents_ExcludesArchivedProjectChildren(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	pa := createProject(ctx, t, d, "alpha")
	pb := createProject(ctx, t, d, "beta")
	parent := makeIssue(t, ctx, d, pa.ID, "parent", "tester")
	liveChild := makeIssue(t, ctx, d, pa.ID, "live child", "tester")
	archivedChild := makeIssue(t, ctx, d, pb.ID, "archived child", "tester")
	makeLink(ctx, t, d, liveChild.ID, parent.ID, "parent")
	makeLink(ctx, t, d, archivedChild.ID, parent.ID, "parent")
	archiveProjectByID(ctx, t, d, pb.ID)

	got, err := d.ChildCountsByParents(ctx, []int64{parent.ID})
	require.NoError(t, err)
	assert.Equal(t, db.ChildCounts{Open: 1, Total: 1}, got[parent.ID],
		"archived-project child must not count toward open or total")
}
