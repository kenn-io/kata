package sqlitestore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSearchFTS_PageMatchedInPreservesComments(t *testing.T) {
	page := []db.SearchCandidate{{
		Issue:     db.Issue{ID: 1, Title: "login", Body: "browser"},
		MatchedIn: []string{"comments"},
	}}
	require.NoError(t, pageMatchedIn(t.Context(), `"login" OR "browser"`, page))
	require.Equal(t, []string{"title", "body", "comments"}, page[0].MatchedIn)
}
