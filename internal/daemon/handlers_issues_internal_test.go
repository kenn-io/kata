package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type lookalikeQueryRecordingStore struct {
	lookalikeCandidateStore
	query      string
	titleQuery string
	titleBody  string
}

func (s *lookalikeQueryRecordingStore) SearchFTSAny(_ context.Context, params db.SearchFTSParams) ([]db.SearchCandidate, error) {
	s.query = params.Query
	s.titleQuery = params.TitleQuery
	s.titleBody = params.TitleBody
	return nil, nil
}

func TestRunLookalikeCheckBoundsSearchQuery(t *testing.T) {
	store := &lookalikeQueryRecordingStore{}
	in := &api.CreateIssueRequest{ProjectID: 7}
	in.Body.Title = "full issue title"
	in.Body.Body = strings.Repeat("界", 499) + "留 discard-this-suffix"

	err := runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project")

	require.NoError(t, err)
	assert.Contains(t, store.query, in.Body.Title)
	assert.Equal(t, in.Body.Body, store.titleBody)
	assert.Equal(t, in.Body.Title, store.titleQuery)
	assert.Contains(t, store.query, "留")
	assert.NotContains(t, store.query, "discard-this-suffix")
	assert.True(t, utf8.ValidString(store.query))

	store = &lookalikeQueryRecordingStore{}
	in.Body.Title = strings.Repeat("界", 499) + "留 discarded-title-suffix"
	in.Body.Body = "plain body"

	err = runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project")

	require.NoError(t, err)
	assert.Contains(t, store.query, "plain body")
	assert.Equal(t, "plain body", store.titleBody)
	assert.Contains(t, store.query, "留")
	assert.NotContains(t, store.query, "discarded-title-suffix")
	assert.Equal(t, strings.Repeat("界", 499)+"留", store.titleQuery)
	assert.True(t, utf8.ValidString(store.query))
}

type lookalikeCandidateStore struct {
	db.Storage
	candidates []db.SearchCandidate
	listIssues func(db.ListIssuesParams) ([]db.Issue, error)
}

func (s *lookalikeCandidateStore) ListIssues(_ context.Context, params db.ListIssuesParams) ([]db.Issue, error) {
	if s.listIssues != nil {
		return s.listIssues(params)
	}
	return nil, nil
}

func (s *lookalikeCandidateStore) SearchFTSAny(_ context.Context, _ db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return s.candidates, nil
}

func TestRunLookalikeCheckExactTitleGate(t *testing.T) {
	for _, tc := range []struct {
		name, title      string
		hits, qualifying int
		lookup           bool
		wantCandidates   int
	}{
		{"full page", "Inventory page crashes", 20, 19, true, 20},
		{"underfull page", "Inventory page crashes", 19, 0, false, 0},
		{"scorer stop word remains searchable", "the", 0, 0, false, 0},
		{"Unicode", "𞤢", 0, 0, true, 20},
		{"PostgreSQL tag", "<T>", 0, 0, true, 20},
		{"title past search prefix", strings.Repeat("x", 501), 0, 0, true, 20},
		{"twenty qualifying FTS hits", "Inventory page crashes", 20, 20, false, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &api.CreateIssueRequest{ProjectID: 7}
			in.Body.Title = tc.title
			in.Body.Body = "apples bananas cherries dates elderberries figs grapes hazelnuts"
			scope := &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: "01HAAAAAAAAAAAAAAAAAAAAAAA", RootIssueUID: "01HBBBBBBBBBBBBBBBBBBBBBBB"}
			ctx := WithPrincipal(t.Context(), Principal{Kind: PrincipalDBToken, Scope: scope})
			store := &lookalikeCandidateStore{}
			for i := range tc.hits {
				body := "apples"
				if i < tc.qualifying {
					body = in.Body.Body
					if i == 0 {
						body = ""
					}
				}
				store.candidates = append(store.candidates, db.SearchCandidate{Issue: db.Issue{UID: fmt.Sprintf("fts-%d", i), Title: tc.title, Body: body}})
			}
			calls := 0
			store.listIssues = func(params db.ListIssuesParams) ([]db.Issue, error) {
				calls++
				require.True(t, tc.lookup, "unexpected exact-title lookup")
				require.Equal(t, tc.title, params.ExactTitle)
				require.Equal(t, int64(7), params.ProjectID)
				require.Equal(t, scope, params.IssueScope)
				require.Equal(t, 20, params.Limit)
				require.True(t, params.ShortestBodyFirst)
				rows := make([]db.Issue, 20)
				for i := range rows {
					rows[i] = db.Issue{UID: fmt.Sprintf("exact-%d", i), Title: tc.title}
				}
				if len(store.candidates) > 0 {
					rows[0] = store.candidates[0].Issue
				}
				return rows, nil
			}
			err := runLookalikeCheck(ctx, ServerConfig{DB: store}, in, "example-project")
			if tc.lookup {
				assert.Equal(t, 1, calls)
			} else {
				assert.Zero(t, calls)
			}
			if tc.wantCandidates == 0 {
				require.NoError(t, err)
				return
			}
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, "duplicate_candidates", apiErr.Code)
			matches := apiErr.Data["candidates"].([]map[string]any)
			assert.Len(t, matches, tc.wantCandidates)
			seen := map[any]bool{}
			for _, match := range matches {
				assert.False(t, seen[match["uid"]], "duplicate candidate %v", match["uid"])
				seen[match["uid"]] = true
			}
		})
	}
}

func closedLookalike(title, body, uid string) db.SearchCandidate {
	done := "done"
	return db.SearchCandidate{
		Issue: db.Issue{
			UID:          uid,
			ShortID:      uid[:4],
			Title:        title,
			Body:         body,
			Status:       "closed",
			ClosedReason: &done,
		},
	}
}

func TestRunLookalikeCheckConflictMessageCountsClosedCandidates(t *testing.T) {
	title := "printer jams on tray two"
	body := "paper path sensor reports a jam after pickup"
	in := &api.CreateIssueRequest{ProjectID: 3}
	in.Body.Title = title
	in.Body.Body = body

	t.Run("single closed candidate", func(t *testing.T) {
		store := &lookalikeCandidateStore{candidates: []db.SearchCandidate{
			closedLookalike(title, body, "01J5EXAMPLE000000000000EXA"),
		}}

		err := runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project")

		require.Error(t, err)
		var apiErr *api.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 409, apiErr.Status)
		assert.Equal(t, "duplicate_candidates", apiErr.Code)
		assert.Equal(t, "1 existing issue matches this title", apiErr.Message)
		assert.NotContains(t, apiErr.Message, "open")
	})

	t.Run("multiple closed candidates", func(t *testing.T) {
		store := &lookalikeCandidateStore{candidates: []db.SearchCandidate{
			closedLookalike(title, body, "01J5EXAMPLE000000000000EXA"),
			closedLookalike(title, body, "01J5EXAMPLE000000000000EXB"),
		}}

		err := runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project")

		require.Error(t, err)
		var apiErr *api.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 409, apiErr.Status)
		assert.Equal(t, "2 existing issues match this title", apiErr.Message)
		assert.NotContains(t, apiErr.Message, "open")
	})
}
