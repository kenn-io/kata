package sqlitestore

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSearchFTSAny_MatchedInMatchesIndex(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	expected := make(map[int64][][]string)
	for _, fixture := range []struct {
		title, body string
		first, next []string
	}{
		{"Café Login pull-request 42", "secondary", []string{"title"}, []string{"body"}},
		{"secondary", "cafe login pull-request 42", []string{"body"}, []string{"title"}},
		{"Café Login pull-request 42 secondary", "cafe login pull-request 42 secondary", []string{"title", "body"}, []string{"title", "body"}},
	} {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fixture.title, Body: fixture.body, Author: "tester"})
		require.NoError(t, err)
		expected[issue.ID] = [][]string{fixture.first, fixture.next}
	}
	// Both pages reuse issue IDs while swapping the title-only and body-only flags.
	for n, query := range []string{"cafe login pull-request 42", "secondary"} {
		phrases, err := searchablePhrases(ctx, query)
		require.NoError(t, err)
		match := strings.Join(phrases, " OR ")
		hits, err := store.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: query})
		require.NoError(t, err)
		require.Len(t, hits, 3)
		for _, hit := range hits {
			require.Contains(t, expected, hit.Issue.ID)
			require.Equal(t, expected[hit.Issue.ID][n], hit.MatchedIn)
			var indexFlags []string
			for _, column := range []string{"title", "body"} {
				var matched bool
				err := store.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM issues_fts WHERE rowid = ? AND `+column+` MATCH ?)`, hit.Issue.ID, match).Scan(&matched)
				require.NoError(t, err)
				if matched {
					indexFlags = append(indexFlags, column)
				}
			}
			require.Equal(t, indexFlags, hit.MatchedIn)
		}
	}
}
