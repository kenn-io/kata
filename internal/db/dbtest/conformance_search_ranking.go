package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/similarity"
)

func checkDuplicateSurvivesHigherWordCountDistractors(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	// Repeated body terms raise relevance in both backends while keeping the
	// duplicate's distinct term count below the distractors'.
	duplicate, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Author: "tester", Title: "fix login callback", Body: "session timeout session timeout",
	})
	require.NoError(t, err)
	for range 22 {
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Author: "tester", Title: "fix session",
			Body: "timeout now login callback " + strings.Repeat("unrelated log detail ", 300),
		})
		require.NoError(t, err)
	}

	// Creation scores only 20 candidates. A duplicate matching five terms must
	// survive retrieval even when long distractors match all six query words.
	hits, err := store.SearchFTSAny(ctx, db.SearchFTSParams{
		ProjectID: project.ID, Query: "fix login callback session timeout now", Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, hits, 20)
	ids := make([]int64, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Issue.ID)
	}
	require.Contains(t, ids, duplicate.ID, "relevance must retain the duplicate before the candidate limit")
	return nil
}

func checkLookalikeKeepsRewrittenBodyDuplicate(t *testing.T, store db.Storage) error {
	const title = "login callback drops session"
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	const common = "request response server client token account"
	const rare = "amber birch cedar delta elm fir grove hazel"
	const retained = "iris juniper maple oak"
	const body = common + " " + rare + " " + retained
	words := strings.Fields(title)
	for n := range 1_024 {
		title, body := "unrelated issue", common
		if n < 24 {
			body += " " + rare + " " + strings.Repeat("padding ", 120)
		} else if n < 124 {
			body += " " + retained + " " + strings.Repeat("padding ", 120)
		} else if n < 124+len(words)*10 {
			title = words[(n-124)/10] + " unrelated issue"
		}
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "tester", Title: title, Body: body})
		require.NoError(t, err)
	}
	original, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Author: "tester", Title: title, Body: common + " " + retained + " rewritten narrative",
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, similarity.Score(title, body, original.Title, original.Body), 0.7)
	for _, titleQuery := range []string{"", title} {
		hits, err := store.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), TitleQuery: titleQuery, Limit: 20})
		require.NoError(t, err)
		ids := make([]int64, 0, len(hits))
		for _, hit := range hits {
			ids = append(ids, hit.Issue.ID)
		}
		require.Contains(t, ids, original.ID, "rewritten-body duplicate must survive with title query %q", titleQuery)
	}
	return nil
}
