package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunUISnapshotScopedAuthorityContract verifies that a scoped browser snapshot
// (non-nil AllowedIssueIDs) never hydrates rows outside the authorization
// candidate set. The scoped read must match an unscoped read filtered to the
// allowed issue IDs by an independent projection: graph issues, every link
// slice, and recurrences included. Graph and link allowlists must be pushed
// into the SQL, and scoped requests must skip recurrence hydration entirely.
func RunUISnapshotScopedAuthorityContract(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	store := open(t)
	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	ctx := context.Background()

	scopeProject, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	otherProject, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)

	root := createCursorIssue(ctx, t, store, scopeProject.ID, "Scoped root")
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: scopeProject.ID, Title: "Scoped child", Author: "user-a",
		Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}},
	})
	require.NoError(t, err)
	hidden := createCursorIssue(ctx, t, store, scopeProject.ID, "Hidden sibling")
	foreign := createCursorIssue(ctx, t, store, otherProject.ID, "Foreign issue")

	createCursorLink(ctx, t, store, child, hidden, "blocks")
	createCursorLink(ctx, t, store, child, foreign, "related")
	// The child already carries its parent link (child -> root) from creation;
	// adding another parent link here would collide with it.
	for _, project := range []db.Project{scopeProject, otherProject} {
		_, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
			ProjectID: project.ID, Actor: "user-a", Rule: "FREQ=WEEKLY",
			DTStart: "2026-08-01", Timezone: "UTC",
			Template: db.RecurrenceTemplate{Title: "Weekly review", Metadata: jsontext.Value(`{}`)},
		})
		require.NoError(t, err)
	}

	allowed := []int64{root.ID, child.ID}
	allowedSet := make(map[int64]struct{}, len(allowed))
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}

	base := db.UISnapshotQuery{
		View: "all-open", SelectedIssueUID: child.UID,
		IncludeGraph: true, IncludeHistory: true,
	}
	unscoped, err := uiStore.ReadUISnapshot(ctx, base)
	require.NoError(t, err)
	scopedQuery := base
	scopedQuery.AllowedIssueIDs = allowed
	scoped, err := uiStore.ReadUISnapshot(ctx, scopedQuery)
	require.NoError(t, err)

	require.Equal(t, unscoped.Cursor, scoped.Cursor)

	// Invariants: nothing outside the allowlist may surface in any hydrated
	// collection of the scoped read.
	requireSubset(t, allowedSet, scoped.Issues)
	requireSubset(t, allowedSet, scoped.GraphIssues)
	requireLinksInsideAllowlist(t, allowedSet, scoped.CollectionLinks)
	requireLinksInsideAllowlist(t, allowedSet, scoped.SelectedLinks)
	requireLinksInsideAllowlist(t, allowedSet, scoped.GraphLinks)
	require.Empty(t, scoped.Recurrences,
		"scoped requests must skip recurrence hydration")

	// Parity: the scoped read must equal an unscoped read narrowed by an
	// independent allowlist projection. Projects are compared separately:
	// catalog narrowing is the handler's job, not the store query's.
	want := filterSnapshotToAllowlist(unscoped, allowed, child)
	require.Equal(t, want.Issues, scoped.Issues)
	require.Equal(t, want.GraphIssues, scoped.GraphIssues)
	require.Equal(t, want.CollectionLinks, scoped.CollectionLinks)
	require.Equal(t, want.SelectedLinks, scoped.SelectedLinks)
	require.Equal(t, want.GraphLinks, scoped.GraphLinks)
	require.Equal(t, want.GraphEdges, scoped.GraphEdges)
	require.Equal(t, want.GraphUnresolvedRefs, scoped.GraphUnresolvedRefs)
	require.Equal(t, want.Recurrences, scoped.Recurrences)
	require.Equal(t, want.SelectedIssue, scoped.SelectedIssue)
	require.Equal(t, want.SelectedState, scoped.SelectedState)
	require.Equal(t, want.Comments, scoped.Comments)
	require.Equal(t, want.SelectedLabels, scoped.SelectedLabels)
	require.Equal(t, want.History, scoped.History)

	// An empty allowlist fails closed for every hydrated collection. The
	// selected issue itself stays keyed to the handler-authorized
	// SelectedIssueUID, but its links must still respect the allowlist.
	emptyQuery := base
	emptyQuery.AllowedIssueIDs = []int64{}
	empty, err := uiStore.ReadUISnapshot(ctx, emptyQuery)
	require.NoError(t, err)
	require.Empty(t, empty.Issues)
	require.Empty(t, empty.GraphIssues)
	require.Empty(t, empty.CollectionLinks)
	require.Empty(t, empty.SelectedLinks)
	require.Empty(t, empty.GraphLinks)
}

func requireSubset(t *testing.T, allowed map[int64]struct{}, issues []db.UIIssue) {
	t.Helper()
	for _, issue := range issues {
		if _, ok := allowed[issue.ID]; !ok {
			t.Fatalf("issue %s (id %d) hydrated outside the allowed set", issue.UID, issue.ID)
		}
	}
}

func requireLinksInsideAllowlist(t *testing.T, allowed map[int64]struct{}, links []db.UILink) {
	t.Helper()
	for _, link := range links {
		_, fromOK := allowed[link.FromIssueID]
		_, toOK := allowed[link.ToIssueID]
		if !fromOK || !toOK {
			t.Fatalf("link %d->%d hydrated with an endpoint outside the allowed set",
				link.FromIssueID, link.ToIssueID)
		}
	}
}

// filterSnapshotToAllowlist independently narrows an unscoped snapshot the
// way the daemon's scoped projection does: keep only allowed issues, drop
// links touching any other endpoint, and clear recurrence data.
func filterSnapshotToAllowlist(
	data db.UISnapshotData, allowed []int64, selected db.Issue,
) db.UISnapshotData {
	allowedSet := make(map[int64]struct{}, len(allowed))
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}
	keepIssues := func(issues []db.UIIssue) []db.UIIssue {
		out := []db.UIIssue{}
		for _, issue := range issues {
			if _, ok := allowedSet[issue.ID]; ok {
				out = append(out, issue)
			}
		}
		return out
	}
	keepLinks := func(links []db.UILink) []db.UILink {
		out := []db.UILink{}
		for _, link := range links {
			_, fromOK := allowedSet[link.FromIssueID]
			_, toOK := allowedSet[link.ToIssueID]
			if fromOK && toOK {
				out = append(out, link)
			}
		}
		return out
	}
	out := db.UISnapshotData{
		Cursor: data.Cursor, SelectedState: data.SelectedState,
		Issues: keepIssues(data.Issues), GraphIssues: keepIssues(data.GraphIssues),
		CollectionLinks: keepLinks(data.CollectionLinks),
		SelectedLinks:   keepLinks(data.SelectedLinks), GraphLinks: keepLinks(data.GraphLinks),
		GraphEdges:          []db.UIGraphEdge{},
		GraphUnresolvedRefs: []db.UIGraphUnresolvedRef{},
		Recurrences:         []db.Recurrence{},
		Comments:            data.Comments, SelectedLabels: data.SelectedLabels,
		History: data.History,
	}
	if _, ok := allowedSet[selected.ID]; ok {
		for _, candidate := range data.Issues {
			if candidate.ID == selected.ID {
				value := candidate
				out.SelectedIssue = &value
				break
			}
		}
	}
	if out.SelectedIssue == nil && data.SelectedIssue != nil {
		if _, ok := allowedSet[data.SelectedIssue.ID]; ok {
			value := *data.SelectedIssue
			out.SelectedIssue = &value
		}
	}
	return out
}
