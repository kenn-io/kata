package pgstore_test

import (
	"context"
	"testing"

	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestSearchFTSScopeFiltersAndTieBreak(t *testing.T) {
	if testing.Short() {
		t.Skip("requires isolated PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "root", Author: "example-author"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "café login", Body: "session expired", Author: "example-author",
		Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}, Labels: []string{"bug"},
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: child.ID, Body: "browser reset", Author: "example-author"})
	require.NoError(t, err)
	sibling, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "café login", Body: "session expired", Author: "example-author", Labels: []string{"bug"},
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: sibling.ID, Body: "browser reset", Author: "example-author"})
	require.NoError(t, err)
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){
		"all": store.SearchFTS, "any": store.SearchFTSAny,
	} {
		t.Run(name, func(t *testing.T) {
			for query, fields := range map[string][]string{
				"cafe": {"title"}, "session": {"body"}, "browser": {"comments"},
				"login session browser": {"title", "body", "comments"},
			} {
				if name == "any" {
					if query == "browser" {
						fields = nil
					} else if query == "login session browser" {
						fields = []string{"title", "body"}
					}
				}
				hits, err := search(ctx, db.SearchFTSParams{
					ProjectID: project.ID, Query: query, Limit: 1, Status: "open", Labels: []string{"BUG"},
					IssueScope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID},
				})
				require.NoError(t, err)
				if fields == nil {
					require.Empty(t, hits, "comment-only matches don't enter look-alike candidates")
					continue
				}
				require.Len(t, hits, 1, "query %q must match the scoped child", query)
				require.Equal(t, child.UID, hits[0].Issue.UID)
				require.Equal(t, fields, hits[0].MatchedIn)
				require.Equal(t, child.Revision, hits[0].Issue.Revision)
			}
			hits, err := search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "login", Limit: 1})
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, sibling.UID, hits[0].Issue.UID, "equal-score candidates retain the descending ID tie-break before LIMIT")
			hits, err = search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "login", Limit: 1, AllowedIssueIDs: []int64{child.ID}})
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, child.UID, hits[0].Issue.UID)
			hits, err = search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "login", Limit: 1, ExcludeLabels: []string{"BUG"}})
			require.NoError(t, err)
			require.Empty(t, hits)
		})
	}
}

func TestSearchFTS_PunctuationLexemes(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "login http://a.example/x'y", Body: "browser", Author: "tester"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "timeout"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, query string
		any         bool
		fields      []string
	}{
		{"all_across_documents", "login,timeout", false, []string{"title", "comments"}},
		{"any_lexeme", "login,absent", true, []string{"title"}},
		{"field_flags", "login,browser", false, []string{"title", "body"}},
		{"distinct_lexemes", "login,login timeout", false, []string{"title", "comments"}},
		{"literal_url_all", "http://a.example/x'y", false, []string{"title"}},
		{"literal_url_any", "http://a.example/x'y", true, []string{"title"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			search := store.SearchFTS
			if tc.any {
				search = store.SearchFTSAny
			}
			hits, err := search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: tc.query})
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, issue.ID, hits[0].Issue.ID)
			require.Equal(t, tc.fields, hits[0].MatchedIn)
		})
	}
}
