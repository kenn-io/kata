package authorname

import (
	"slices"
	"testing"

	"go.kenn.io/kata/pkg/client/generated"
)

func TestGroupDigest(t *testing.T) {
	t.Run("sorts displayed actors and issues", func(t *testing.T) {
		const member = "11111111-1111-4111-8111-111111111111"
		const laptop = "22222222-2222-4222-8222-222222222222"
		const desktop = "33333333-3333-4333-8333-333333333333"
		actors := []generated.DigestActorEntry{
			{Actor: "Ana 2"},
			{Actor: "Ana [member:" + member + "; enrollment:" + laptop + "]", Issues: []generated.DigestIssueActions{{ProjectID: 2, IssueUID: "b"}}},
			{Actor: "Ana [member:" + member + "; enrollment:" + desktop + "]", Issues: []generated.DigestIssueActions{{ProjectID: 1, IssueUID: "z"}, {ProjectID: 2, IssueUID: "a"}}},
		}
		got := GroupDigest(actors)
		if len(got) != 2 || got[0].Actor != "Ana" || got[1].Actor != "Ana 2" {
			t.Fatalf("actors: %+v", got)
		}
		issues := got[0].Issues
		if len(issues) != 3 || issues[0].ProjectID != 1 || issues[0].IssueUID != "z" || issues[1].ProjectID != 2 || issues[1].IssueUID != "a" || issues[2].ProjectID != 2 || issues[2].IssueUID != "b" {
			t.Fatalf("issues: %+v", issues)
		}
	})
}

func TestGroupDigestPreservesConflictingIssueActions(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	actors := []generated.DigestActorEntry{
		{Actor: "Ana", Totals: generated.DigestTotals{Commented: 1}, Issues: []generated.DigestIssueActions{{ProjectID: 1, IssueUID: "a", Actions: []string{"priority:1", "assigned:alice", "closed:done"}}}},
		{Actor: "Ana [member:" + id + "; enrollment:" + id + "]", Totals: generated.DigestTotals{Commented: 2}, Issues: []generated.DigestIssueActions{{ProjectID: 1, IssueUID: "a", Actions: []string{"priority:2", "assigned:bob", "closed:duplicate"}}}},
	}
	groups := GroupDigest(actors)
	if len(groups) != 1 || groups[0].Actor != "Ana" || groups[0].Totals.Commented != 3 {
		t.Fatalf("group: %+v", groups)
	}
	issues := groups[0].Issues
	if len(issues) != 2 || !slices.Equal(issues[0].Actions, actors[0].Issues[0].Actions) || !slices.Equal(issues[1].Actions, actors[1].Issues[0].Actions) {
		t.Fatalf("issue actions: %+v", issues)
	}
}
