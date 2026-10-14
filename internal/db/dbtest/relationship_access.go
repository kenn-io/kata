package dbtest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// relationshipAccessFixture links issues in a visible project to issues in a
// project the restricted reader cannot see. Every link kind has one edge whose
// far endpoint is hidden and one whose endpoints are both visible, so a
// relationship read that drops its endpoint authorization returns the hidden
// edge and a read that drops everything returns no visible edge either.
type relationshipAccessFixture struct {
	visible, hidden db.Project

	childOfHidden, childOfVisible, visibleParent, parentOfHiddenOnly db.Issue
	blockedByHidden, blockedByVisible, visibleBlocker                db.Issue
	relatedMixed, relatedPeer, relatedHiddenOnly                     db.Issue

	hiddenParent, hiddenChild, hiddenChild2, hiddenBlocker, hiddenBlocked db.Issue
	hiddenPeer, hiddenPeer2                                               db.Issue
	// hiddenEarlyPeer is created before every visible issue so a stored
	// related link can put the hidden endpoint on the canonical from side
	// (related links require from_issue_id < to_issue_id).
	hiddenEarlyPeer db.Issue
}

func newRelationshipAccessFixture(ctx context.Context, t *testing.T, store db.Storage) relationshipAccessFixture {
	t.Helper()
	var f relationshipAccessFixture
	var err error
	f.visible, err = store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	f.hidden, err = store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	create := func(project db.Project, title string) db.Issue {
		issue, _, createErr := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "relationship-author"})
		require.NoError(t, createErr)
		return issue
	}
	f.hiddenEarlyPeer = create(f.hidden, "hidden peer on the from side")
	f.childOfHidden = create(f.visible, "child of hidden parent")
	f.childOfVisible = create(f.visible, "child of visible parent")
	f.visibleParent = create(f.visible, "visible parent")
	f.parentOfHiddenOnly = create(f.visible, "parent of a hidden child")
	f.blockedByHidden = create(f.visible, "blocked by hidden blocker")
	f.blockedByVisible = create(f.visible, "blocked by visible blocker")
	f.visibleBlocker = create(f.visible, "visible blocker")
	f.relatedMixed = create(f.visible, "related to visible and hidden peers")
	f.relatedPeer = create(f.visible, "visible related peer")
	f.relatedHiddenOnly = create(f.visible, "related to a hidden peer")
	f.hiddenParent = create(f.hidden, "hidden parent")
	f.hiddenChild = create(f.hidden, "hidden child")
	f.hiddenChild2 = create(f.hidden, "hidden only child")
	f.hiddenBlocker = create(f.hidden, "hidden blocker")
	f.hiddenBlocked = create(f.hidden, "hidden blocked")
	f.hiddenPeer = create(f.hidden, "hidden peer")
	f.hiddenPeer2 = create(f.hidden, "hidden only peer")
	for _, link := range []db.CreateLinkParams{
		{FromIssueID: f.childOfHidden.ID, ToIssueID: f.hiddenParent.ID, Type: "parent"},
		{FromIssueID: f.childOfVisible.ID, ToIssueID: f.visibleParent.ID, Type: "parent"},
		{FromIssueID: f.hiddenChild.ID, ToIssueID: f.visibleParent.ID, Type: "parent"},
		{FromIssueID: f.hiddenChild2.ID, ToIssueID: f.parentOfHiddenOnly.ID, Type: "parent"},
		{FromIssueID: f.hiddenBlocker.ID, ToIssueID: f.blockedByHidden.ID, Type: "blocks"},
		{FromIssueID: f.visibleBlocker.ID, ToIssueID: f.blockedByVisible.ID, Type: "blocks"},
		{FromIssueID: f.visibleBlocker.ID, ToIssueID: f.hiddenBlocked.ID, Type: "blocks"},
		{FromIssueID: f.relatedMixed.ID, ToIssueID: f.relatedPeer.ID, Type: "related"},
		{FromIssueID: f.relatedMixed.ID, ToIssueID: f.hiddenPeer.ID, Type: "related"},
		{FromIssueID: f.hiddenEarlyPeer.ID, ToIssueID: f.relatedPeer.ID, Type: "related"},
		{FromIssueID: f.relatedHiddenOnly.ID, ToIssueID: f.hiddenPeer2.ID, Type: "related"},
	} {
		link.Author = "relationship-author"
		_, err := store.CreateLink(ctx, link)
		require.NoError(t, err, "create %s link", link.Type)
	}
	return f
}

func (f relationshipAccessFixture) visibleIDs() []int64 {
	return []int64{
		f.childOfHidden.ID, f.childOfVisible.ID, f.visibleParent.ID, f.parentOfHiddenOnly.ID,
		f.blockedByHidden.ID, f.blockedByVisible.ID, f.visibleBlocker.ID,
		f.relatedMixed.ID, f.relatedPeer.ID, f.relatedHiddenOnly.ID,
	}
}

// checkRestrictedRelationshipReads proves that a reader admitted to one
// project never learns a link endpoint in another project through any
// relationship projection, while in-project relationships still resolve. A
// hidden open blocker does not gate readiness for that reader.
func checkRestrictedRelationshipReads(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	f := newRelationshipAccessFixture(ctx, t, store)
	scoped := db.WithAuthorizedProjects(ctx, []string{f.visible.UID})

	full, err := store.RelationshipsByIssues(ctx, f.visibleIDs())
	if err != nil {
		return fmt.Errorf("unrestricted relationships: %w", err)
	}
	require.NotNil(t, full[f.childOfHidden.ID].ParentIssueID)
	assert.Equal(t, f.hiddenParent.ID, *full[f.childOfHidden.ID].ParentIssueID)
	assert.Equal(t, db.ChildCounts{Open: 2, Total: 2}, full[f.visibleParent.ID].Children)
	assert.True(t, full[f.blockedByHidden.ID].ActivelyBlocked)
	assert.Equal(t, []int64{f.hiddenPeer2.ID}, full[f.relatedHiddenOnly.ID].Related)
	assert.Equal(t, []int64{f.hiddenEarlyPeer.ID, f.relatedMixed.ID}, full[f.relatedPeer.ID].Related, "a hidden peer on the from side is visible unrestricted")

	rel, err := store.RelationshipsByIssues(scoped, f.visibleIDs())
	if err != nil {
		return fmt.Errorf("restricted relationships: %w", err)
	}
	assert.Nil(t, rel[f.childOfHidden.ID].ParentIssueID, "hidden parent")
	require.NotNil(t, rel[f.childOfVisible.ID].ParentIssueID, "visible parent")
	assert.Equal(t, f.visibleParent.ID, *rel[f.childOfVisible.ID].ParentIssueID)
	assert.Equal(t, db.ChildCounts{Open: 1, Total: 1}, rel[f.visibleParent.ID].Children, "hidden child is not counted")
	assert.Equal(t, db.ChildCounts{}, rel[f.parentOfHiddenOnly.ID].Children)
	assert.Empty(t, rel[f.blockedByHidden.ID].BlockedBy, "hidden blocker")
	assert.False(t, rel[f.blockedByHidden.ID].ActivelyBlocked, "hidden blocker does not block")
	assert.Equal(t, []int64{f.visibleBlocker.ID}, rel[f.blockedByVisible.ID].BlockedBy)
	assert.True(t, rel[f.blockedByVisible.ID].ActivelyBlocked)
	assert.Equal(t, []int64{f.blockedByVisible.ID}, rel[f.visibleBlocker.ID].Blocks, "hidden blocked issue")
	assert.Equal(t, []int64{f.relatedPeer.ID}, rel[f.relatedMixed.ID].Related, "hidden related peer")
	assert.Equal(t, []int64{f.relatedMixed.ID}, rel[f.relatedPeer.ID].Related, "hidden peer on the from side")
	assert.Empty(t, rel[f.relatedHiddenOnly.ID].Related)

	children, err := store.ChildrenOfIssue(scoped, f.visibleParent.ID)
	if err != nil {
		return fmt.Errorf("restricted children: %w", err)
	}
	assert.Equal(t, []int64{f.childOfVisible.ID}, issueIDs(children))
	children, err = store.ChildrenOfIssue(scoped, f.parentOfHiddenOnly.ID)
	if err != nil {
		return fmt.Errorf("restricted hidden-only children: %w", err)
	}
	assert.Empty(t, children)
	parents, err := store.ParentShortIDsByIssues(scoped, []int64{f.childOfHidden.ID, f.childOfVisible.ID})
	if err != nil {
		return fmt.Errorf("restricted parent short ids: %w", err)
	}
	assert.Equal(t, map[int64]string{f.childOfVisible.ID: f.visibleParent.ShortID}, parents)

	ready, err := store.ReadyIssues(ctx, f.visible.ID, 0, db.ReadyIssuesFilter{})
	if err != nil {
		return fmt.Errorf("unrestricted ready: %w", err)
	}
	assert.NotContains(t, issueIDs(ready), f.blockedByHidden.ID, "an unrestricted reader sees the hidden blocker gate readiness")
	ready, err = store.ReadyIssues(scoped, f.visible.ID, 0, db.ReadyIssuesFilter{})
	if err != nil {
		return fmt.Errorf("restricted ready: %w", err)
	}
	assert.Contains(t, issueIDs(ready), f.blockedByHidden.ID, "a hidden blocker does not gate readiness")
	assert.NotContains(t, issueIDs(ready), f.blockedByVisible.ID)
	global, err := store.ReadyIssuesGlobal(scoped, 0, db.ReadyIssuesFilter{})
	if err != nil {
		return fmt.Errorf("restricted ready global: %w", err)
	}
	var globalIDs []int64
	for _, issue := range global {
		assert.Equal(t, f.visible.Name, issue.ProjectName)
		globalIDs = append(globalIDs, issue.ID)
	}
	assert.Contains(t, globalIDs, f.blockedByHidden.ID)
	assert.NotContains(t, globalIDs, f.blockedByVisible.ID)
	return nil
}

// RunUISnapshotProjectAuthorityRelationshipsContract verifies that the
// browser snapshot's synthetic ready status and relationship filters honor the
// caller's project boundary: a link whose far endpoint lives in a project
// outside that boundary neither blocks readiness nor matches a filter.
func RunUISnapshotProjectAuthorityRelationshipsContract(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	store := open(t)
	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	ctx := context.Background()
	f := newRelationshipAccessFixture(ctx, t, store)
	scoped := db.WithAuthorizedProjects(ctx, []string{f.visible.UID})
	uids := func(ctx context.Context, query db.UISnapshotQuery) []string {
		query.View = "all-open"
		snapshot, err := uiStore.ReadUISnapshot(ctx, query)
		require.NoError(t, err)
		var out []string
		for _, issue := range snapshot.Issues {
			out = append(out, issue.UID)
		}
		slices.Sort(out)
		return out
	}
	sorted := func(issues ...db.Issue) []string {
		var out []string
		for _, issue := range issues {
			out = append(out, issue.UID)
		}
		slices.Sort(out)
		return out
	}

	unrestricted := uids(ctx, db.UISnapshotQuery{Relationships: []string{"blocked_by"}})
	assert.Equal(t, sorted(f.blockedByHidden, f.blockedByVisible, f.hiddenBlocked), unrestricted)
	ready := uids(ctx, db.UISnapshotQuery{Statuses: []string{"ready"}})
	assert.NotContains(t, ready, f.blockedByHidden.UID, "an unrestricted reader sees the hidden blocker gate readiness")

	ready = uids(scoped, db.UISnapshotQuery{Statuses: []string{"ready"}})
	assert.Contains(t, ready, f.blockedByHidden.UID, "a hidden blocker does not gate readiness")
	assert.NotContains(t, ready, f.blockedByVisible.UID)
	assert.NotContains(t, ready, f.hiddenBlocked.UID)
	assert.Equal(t, sorted(f.blockedByVisible), uids(scoped, db.UISnapshotQuery{Relationships: []string{"blocked_by"}}))
	assert.Equal(t, sorted(f.visibleBlocker), uids(scoped, db.UISnapshotQuery{Relationships: []string{"blocks"}}))
	assert.Equal(t, sorted(f.childOfVisible), uids(scoped, db.UISnapshotQuery{Relationships: []string{"parent"}}))
	assert.Equal(t, sorted(f.visibleParent), uids(scoped, db.UISnapshotQuery{Relationships: []string{"child"}}))
	assert.Equal(t, sorted(f.relatedMixed, f.relatedPeer), uids(scoped, db.UISnapshotQuery{Relationships: []string{"related"}}))
}
