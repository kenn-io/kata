package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSearchFTSAny_MatchedInMatchesIndex(t *testing.T) {
	store, ctx, project := setupTestProject(t)
	expected := make(map[int64][][]string)
	for _, fixture := range []struct {
		title, body string
		first, next []string
	}{
		{"Café Login pull-request 42", "secondary", []string{"title"}, []string{"body"}},
		{"secondary", "cafe login pull-request 42", []string{"body"}, []string{"title"}},
		{"Café Login pull-request 42 secondary", "cafe login pull-request 42 secondary", []string{"title", "body"}, []string{"title", "body"}},
	} {
		issue := createTesterIssueWithBody(ctx, t, store, project.ID, fixture.title, fixture.body)
		expected[issue.ID] = [][]string{fixture.first, fixture.next}
	}
	// Both pages reuse issue IDs while swapping the title-only and body-only flags.
	for n, query := range []string{"cafe login pull-request 42", "secondary"} {
		hits, err := store.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: query})
		require.NoError(t, err)
		require.Len(t, hits, 3)
		for _, hit := range hits {
			require.Contains(t, expected, hit.Issue.ID)
			require.Equal(t, expected[hit.Issue.ID][n], hit.MatchedIn)
		}
	}
}
