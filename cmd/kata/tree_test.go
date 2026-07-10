package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ids extracts the ID sequence from rows, for order assertions.
func ids(rows []issueRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// prefixes extracts the TreePrefix sequence from rows.
func prefixes(rows []issueRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.TreePrefix
	}
	return out
}

// keyed builds the keys slice for treeRows from row IDs, for tests where
// display ID and tree key coincide.
func keyed(rows []issueRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// TestTreeRows_GroupsChildrenUnderParent pins the core grouping: children
// move directly beneath their parent, non-last children get "├─ ", the
// last child gets "└─ ", and roots keep list order with no prefix.
func TestTreeRows_GroupsChildrenUnderParent(t *testing.T) {
	rows := []issueRow{
		{ID: "p1"},
		{ID: "solo"},
		{ID: "c1"},
		{ID: "c2"},
	}
	got := treeRows(rows, keyed(rows), []string{"", "", "p1", "p1"})

	assert.Equal(t, []string{"p1", "c1", "c2", "solo"}, ids(got))
	assert.Equal(t, []string{"", "├─ ", "└─ ", ""}, prefixes(got))
}

// TestTreeRows_OrphanedChildRendersFlat pins the filter-mismatch fallback:
// a child whose parent is not in the fetched set renders flat at top
// level, in list order, with no connector.
func TestTreeRows_OrphanedChildRendersFlat(t *testing.T) {
	rows := []issueRow{
		{ID: "a"},
		{ID: "orphan"},
	}
	got := treeRows(rows, keyed(rows), []string{"", "gone"})

	assert.Equal(t, []string{"a", "orphan"}, ids(got))
	assert.Equal(t, []string{"", ""}, prefixes(got))
}

// TestTreeRows_KeysAreQualifiedNotShort pins the cross-project collision
// guard: rows are keyed by qualified id, so a foreign parent whose
// SHORT id collides with a local issue must NOT capture the child —
// the child renders flat.
func TestTreeRows_KeysAreQualifiedNotShort(t *testing.T) {
	rows := []issueRow{
		{ID: "abcd"}, // local issue whose short id collides with the foreign parent
		{ID: "wxyz"}, // child of other-project#abcd, not local abcd
	}
	keys := []string{"local#abcd", "local#wxyz"}
	got := treeRows(rows, keys, []string{"", "other#abcd"})

	assert.Equal(t, []string{"abcd", "wxyz"}, ids(got))
	assert.Equal(t, []string{"", ""}, prefixes(got),
		"foreign parent must not match a colliding local short id")
}

// TestTreeRows_RendersNestedChainsRecursively pins recursion: a
// grandchild nests under its parent with the ancestor continuation
// prefix ("   " when the ancestor was a last child, "│  " otherwise).
func TestTreeRows_RendersNestedChainsRecursively(t *testing.T) {
	rows := []issueRow{
		{ID: "root"},
		{ID: "mid1"},
		{ID: "mid2"},
		{ID: "leaf"},
	}
	// root -> mid1, mid2; mid1 -> leaf. mid1 is NOT the last child, so
	// leaf's prefix continues the "│" rail.
	got := treeRows(rows, keyed(rows), []string{"", "root", "root", "mid1"})

	assert.Equal(t, []string{"root", "mid1", "leaf", "mid2"}, ids(got))
	assert.Equal(t, []string{"", "├─ ", "│  └─ ", "└─ "}, prefixes(got))
}

// TestTreeRows_LastChildDescendantsUseBlankRail pins the counterpart:
// descendants of a LAST child indent with spaces, not the "│" rail.
func TestTreeRows_LastChildDescendantsUseBlankRail(t *testing.T) {
	rows := []issueRow{
		{ID: "root"},
		{ID: "mid"},
		{ID: "leaf"},
	}
	got := treeRows(rows, keyed(rows), []string{"", "root", "mid"})

	assert.Equal(t, []string{"root", "mid", "leaf"}, ids(got))
	assert.Equal(t, []string{"", "└─ ", "   └─ "}, prefixes(got))
}

// TestTreeRows_ParentCycleDoesNotLoopOrDropRows guards the degenerate
// case: if parent links ever form a cycle, every row still renders
// exactly once (flat is acceptable; hanging or dropping rows is not).
func TestTreeRows_ParentCycleDoesNotLoopOrDropRows(t *testing.T) {
	rows := []issueRow{
		{ID: "a"},
		{ID: "b"},
	}
	got := treeRows(rows, keyed(rows), []string{"b", "a"})

	assert.ElementsMatch(t, []string{"a", "b"}, ids(got))
}
