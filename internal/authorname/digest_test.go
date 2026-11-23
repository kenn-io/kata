package authorname

import (
	"slices"
	"testing"

	"go.kenn.io/kata/pkg/client/generated"
)

func TestGroupDigest(t *testing.T) {
	t.Run("merges issue actions", func(t *testing.T) {
		const id = "11111111-1111-4111-8111-111111111111"
		actors := []generated.DigestActorEntry{
			{Actor: "Ana", Totals: generated.DigestTotals{Commented: 1, Created: 1, Other: 2}, Issues: []generated.DigestIssueActions{
				{ProjectID: 1, IssueUID: "issue-a", IssueShortID: "a", Actions: []string{"created", "commented", "other:2", "labeled:Bug", "labeled:σ"}},
			}},
			{Actor: "Ana [member:" + id + "; enrollment:" + id + "]", Totals: generated.DigestTotals{Commented: 2, Created: 3, Other: 4}, Issues: []generated.DigestIssueActions{
				{ProjectID: 1, IssueUID: "issue-a", IssueShortID: "a", Actions: []string{"created", "CREATED", "commented:2", "labeled:bug", "other:3", "labeled:urgent", "labeled:ς"}},
				{ProjectID: 2, IssueUID: "issue-a", IssueShortID: "a", Actions: []string{"closed:done"}},
			}},
		}
		got := GroupDigest(actors)
		if len(got) != 1 || got[0].Actor != "Ana" || got[0].Totals.Commented != 3 || got[0].Totals.Created != 4 || got[0].Totals.Other != 6 || len(got[0].Issues) != 2 {
			t.Fatalf("grouped digest: %+v", got)
		}
		if !slices.Equal(got[0].Issues[0].Actions, []string{"created", "commented:3", "labeled:Bug", "labeled:σ", "labeled:urgent", "labeled:ς", "other:5"}) {
			t.Fatalf("actions: %v", got[0].Issues[0].Actions)
		}
		if !slices.Equal(got[0].Issues[1].Actions, []string{"closed:done"}) {
			t.Fatalf("other project: %+v", got[0].Issues[1])
		}
		if !slices.Equal(actors[0].Issues[0].Actions, []string{"created", "commented", "other:2", "labeled:Bug", "labeled:σ"}) {
			t.Fatal("input actions changed")
		}
		if actors[1].Actor == "Ana" || len(actors[1].Issues) != 2 {
			t.Fatal("input was changed")
		}
	})
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
	t.Run("orders actions", func(t *testing.T) {
		const id = "11111111-1111-4111-8111-111111111111"
		actors := []generated.DigestActorEntry{
			{Actor: "Ana", Issues: []generated.DigestIssueActions{{ProjectID: 1, IssueUID: "a", Actions: []string{"commented:2", "labeled:bug", "unparent:x", "reopened", "closed:done", "other:2"}}}},
			{Actor: "Ana [member:" + id + "; enrollment:" + id + "]", Issues: []generated.DigestIssueActions{{ProjectID: 1, IssueUID: "a", Actions: []string{"created", "edited", "assigned:worker", "unassigned", "priority:1", "priority_cleared", "unlabeled:bug", "blocks:x", "unblocks:y", "deleted", "restored", "other:3"}}}},
		}
		got := GroupDigest(actors)[0].Issues[0].Actions
		want := []string{"created", "commented:2", "edited", "assigned:worker", "unassigned", "priority:1", "priority_cleared", "labeled:bug", "unlabeled:bug", "blocks:x", "unparent:x", "unblocks:y", "reopened", "closed:done", "deleted", "restored", "other:5"}
		if !slices.Equal(got, want) {
			t.Fatalf("actions: %v, want %v", got, want)
		}
	})
}
