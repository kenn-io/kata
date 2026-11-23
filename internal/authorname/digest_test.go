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
			{Actor: "Ana [member:" + member + "; enrollment:" + laptop + "]", Issues: []generated.DigestIssueActions{{ProjectID: 2, IssueUID: "b", Actions: []string{"edited"}}}},
			{Actor: "Ana [member:" + member + "; enrollment:" + desktop + "]", Issues: []generated.DigestIssueActions{{ProjectID: 1, IssueUID: "z", Actions: []string{"edited"}}, {ProjectID: 2, IssueUID: "a", Actions: []string{"created"}}}},
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

func TestGroupDigestIssueActions(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const otherID = "22222222-2222-4222-8222-222222222222"
	names := []string{"Ana", "Ana [member:" + id + "; enrollment:" + id + "]", "Ana [member:" + id + "; enrollment:" + otherID + "]"}
	issue := func(project int64, actions ...string) generated.DigestIssueActions {
		return generated.DigestIssueActions{ProjectID: project, IssueUID: "a", Actions: actions}
	}
	tests := []struct {
		name        string
		input, want []generated.DigestIssueActions
	}{
		{"compatible", []generated.DigestIssueActions{issue(1, "created", "commented", "labeled:a"), issue(1, "commented:2", "edited", "labeled:a", "labeled:b")}, []generated.DigestIssueActions{issue(1, "created", "commented:3", "edited", "labeled:a", "labeled:b")}},
		{"same priority", []generated.DigestIssueActions{issue(1, "priority:1"), issue(1, "priority:1")}, []generated.DigestIssueActions{issue(1, "priority:1")}},
		{"missing priority", []generated.DigestIssueActions{issue(1, "priority:1"), issue(1, "edited")}, []generated.DigestIssueActions{issue(1, "edited", "priority:1")}},
		{"conflicting assignments", []generated.DigestIssueActions{issue(1, "assigned:alice"), issue(1, "assigned:bob")}, []generated.DigestIssueActions{issue(1, "assigned:alice"), issue(1, "assigned:bob")}},
		{"conflicting priorities", []generated.DigestIssueActions{issue(1, "priority:1"), issue(1, "priority:2")}, []generated.DigestIssueActions{issue(1, "priority:1"), issue(1, "priority:2")}},
		{"conflicting close reasons", []generated.DigestIssueActions{issue(1, "closed:done"), issue(1, "closed:duplicate")}, []generated.DigestIssueActions{issue(1, "closed:done"), issue(1, "closed:duplicate")}},
		{"assignment and unassignment", []generated.DigestIssueActions{issue(1, "assigned:alice"), issue(1, "unassigned")}, []generated.DigestIssueActions{issue(1, "assigned:alice", "unassigned")}},
		{"priority and clear", []generated.DigestIssueActions{issue(1, "priority:1"), issue(1, "priority_cleared")}, []generated.DigestIssueActions{issue(1, "priority:1", "priority_cleared")}},
		{"close and reopen", []generated.DigestIssueActions{issue(1, "closed:done"), issue(1, "reopened")}, []generated.DigestIssueActions{issue(1, "reopened", "closed:done")}},
		{"placeholder and action", []generated.DigestIssueActions{issue(1, "-"), issue(1, "edited")}, []generated.DigestIssueActions{issue(1, "edited")}},
		{"placeholders", []generated.DigestIssueActions{issue(1, "-"), issue(1, "-")}, []generated.DigestIssueActions{issue(1, "-")}},
		{"greedy placement", []generated.DigestIssueActions{issue(1, "priority:1"), issue(1, "priority:2"), issue(1, "edited", "priority:1")}, []generated.DigestIssueActions{issue(1, "edited", "priority:1"), issue(1, "priority:2")}},
		{"different projects", []generated.DigestIssueActions{issue(1, "created"), issue(2, "edited")}, []generated.DigestIssueActions{issue(1, "created"), issue(2, "edited")}},
		{"other counts", []generated.DigestIssueActions{issue(1, "other:2"), issue(1, "other:3")}, []generated.DigestIssueActions{issue(1, "other:5")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actors := make([]generated.DigestActorEntry, len(tt.input))
			for i, row := range tt.input {
				actors[i] = generated.DigestActorEntry{Actor: names[i], Totals: generated.DigestTotals{Commented: 1}, Issues: []generated.DigestIssueActions{row}}
			}
			for range 2 {
				groups := GroupDigest(actors)
				if len(groups) != 1 || groups[0].Actor != "Ana" || groups[0].Totals.Commented != int64(len(tt.input)) {
					t.Fatalf("group: %+v", groups)
				}
				got := groups[0].Issues
				if len(got) != len(tt.want) {
					t.Fatalf("issues: got %+v, want %+v", got, tt.want)
				}
				for i := range got {
					if got[i].ProjectID != tt.want[i].ProjectID || got[i].IssueUID != tt.want[i].IssueUID || !slices.Equal(got[i].Actions, tt.want[i].Actions) {
						t.Errorf("issue %d: got %+v, want %+v", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}
