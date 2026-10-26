package daemon

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

// Decoration must never split accountable actors or change any existing totals
// or issue actions. Project-only events remain ignored, even if tagged.
func TestPropertyDigestTeammateLeavesAccountingUnchanged(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		var rows []db.Event
		count := min(hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1))), 100)
		const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-"
		for range count {
			n := hegel.Draw(ht, hegel.Integers(0, 64))
			handle := make([]byte, n)
			for i := range handle {
				alphabet := chars
				if i == 0 {
					alphabet = chars[:62]
				}
				handle[i] = alphabet[hegel.Draw(ht, hegel.Integers(0, len(alphabet)-1))]
			}
			variant := hegel.Draw(ht, hegel.SampledFrom([]struct{ kind, payload string }{
				{"project.created", `{}`}, {"project.renamed", `{}`},
				{"issue.created", `{"labels":["bug","docs"],"owner":"worker","links":[{"type":"blocks","to_short_id":"j7m2"}]}`},
				{"issue.closed", `{"reason":"done"}`}, {"issue.reopened", `{}`}, {"issue.commented", `{"body":"Finding"}`},
				{"issue.updated", `{"title":"Edited title"}`}, {"issue.assigned", `{"owner":"worker"}`}, {"issue.unassigned", `{}`},
				{"issue.labeled", `{"label":"bug"}`}, {"issue.unlabeled", `{"label":"docs"}`},
				{"issue.linked", `{"type":"blocks","to_short_id":"j7m2"}`}, {"issue.unlinked", `{"type":"blocks","to_short_id":"j7m2"}`},
				{"issue.priority_set", `{"priority":2}`}, {"issue.priority_cleared", `{}`},
				{"issue.soft_deleted", `{}`}, {"issue.restored", `{}`}, {"cron.run.observed", `{}`},
			}))
			actor := hegel.Draw(ht, hegel.Text())
			if hegel.Draw(ht, hegel.Booleans()) {
				actor = hegel.Draw(ht, hegel.SampledFrom([]string{"alice", "bob"}))
			}
			row := db.Event{Actor: actor, Teammate: string(handle), Type: variant.kind, Payload: variant.payload}
			if strings.HasPrefix(variant.kind, "issue.") {
				row.ProjectID = int64(hegel.Draw(ht, hegel.Integers(1, 2)))
				row.ProjectName = fmt.Sprintf("example-project-%d", row.ProjectID)
				index := hegel.Draw(ht, hegel.Integers(0, 2))
				issueUID := fmt.Sprintf("issue-%d-%d", row.ProjectID, index)
				ref := []string{"abc4", "d4ex", "j7m2"}[index]
				row.IssueUID = &issueUID
				row.IssueShortID = &ref
			}
			rows = append(rows, row)
		}
		totals, actors := aggregateDigest(rows)
		untagged := append([]db.Event{}, rows...)
		for i := range untagged {
			untagged[i].Teammate = ""
		}
		expectedTotals, expectedActors := aggregateDigest(untagged)
		require.Equal(ht, expectedTotals, totals)
		for i := range actors {
			actors[i].Teammates = []string{}
		}
		require.Equal(ht, expectedActors, actors)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestDigestIgnoredProjectEventsDoNotContributeTeammates(t *testing.T) {
	_, actors := aggregateDigest([]db.Event{
		{Type: "project.created", Actor: "project-only", Teammate: "ignored-worker"},
		{Type: "project.renamed", Actor: "alice", Teammate: "ignored-worker"},
		{Type: "issue.updated", Actor: "alice", Payload: `{}`},
	})
	require.Len(t, actors, 1)
	require.Equal(t, "alice", actors[0].Actor)
	require.Equal(t, []string{}, actors[0].Teammates)
	raw, err := json.Marshal(actors)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"teammates":[]`)
}

func TestDigestTagSubstitutionPreservesIssueActions(t *testing.T) {
	issueUID, ref := "issue-a", "abc4"
	for _, tags := range [][]string{{"", "", "", "", ""}, {"worker", "worker", "worker", "worker", "worker"}, {"one", "two", "three", "four", "five"}} {
		rows := []db.Event{
			{Type: "issue.created", Actor: "alice", Payload: `{}`},
			{Type: "issue.commented", Actor: "alice", Payload: `{"body":"First finding"}`},
			{Type: "issue.commented", Actor: "alice", Payload: `{"body":"Second finding"}`},
			{Type: "issue.closed", Actor: "alice", Payload: `{"reason":"done"}`},
			{Type: "issue.reopened", Actor: "bob", Payload: `{}`},
		}
		for i := range rows {
			rows[i].IssueUID = &issueUID
			rows[i].IssueShortID = &ref
			rows[i].ProjectID = 1
			rows[i].ProjectName = "example-project"
			rows[i].Teammate = tags[i]
		}
		totals, actors := aggregateDigest(rows)
		require.Equal(t, api.DigestTotals{Created: 1, Commented: 2, Closed: 1, Reopened: 1}, totals)
		require.Len(t, actors, 2)
		require.Equal(t, "alice", actors[0].Actor)
		require.Equal(t, "bob", actors[1].Actor)
		require.Equal(t, api.DigestTotals{Created: 1, Commented: 2, Closed: 1}, actors[0].Totals)
		require.Equal(t, api.DigestTotals{Reopened: 1}, actors[1].Totals)
		require.Equal(t, []api.DigestIssueActions{{ProjectID: 1, ProjectName: "example-project", IssueUID: issueUID, IssueShortID: ref, Actions: []string{"created", "commented:2", "closed:done"}}}, actors[0].Issues)
		require.Equal(t, []api.DigestIssueActions{{ProjectID: 1, ProjectName: "example-project", IssueUID: issueUID, IssueShortID: ref, Actions: []string{"reopened"}}}, actors[1].Issues)
	}
}

func TestDigestCompoundCreateAccountingWithTeammates(t *testing.T) {
	issueUID, ref := "issue-a", "abc4"
	for _, tag := range []string{"", "worker"} {
		totals, actors := aggregateDigest([]db.Event{{Type: "issue.created", Actor: "alice", Teammate: tag, ProjectID: 1, ProjectName: "example-project", IssueUID: &issueUID, IssueShortID: &ref, Payload: `{"labels":["bug","docs"],"owner":"worker"}`}})
		require.Equal(t, api.DigestTotals{Created: 1, Labeled: 2, Assigned: 1}, totals)
		require.Len(t, actors, 1)
		require.Equal(t, totals, actors[0].Totals)
		require.Equal(t, []api.DigestIssueActions{{ProjectID: 1, ProjectName: "example-project", IssueUID: issueUID, IssueShortID: ref, Actions: []string{"created", "assigned:worker", "labeled:bug", "labeled:docs"}}}, actors[0].Issues)
	}
}
