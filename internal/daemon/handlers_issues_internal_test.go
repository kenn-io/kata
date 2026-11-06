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
	query string
}

func (s *lookalikeQueryRecordingStore) SearchFTSAny(_ context.Context, params db.SearchFTSParams) ([]db.SearchCandidate, error) {
	s.query = params.Query
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
	assert.Contains(t, store.query, "留")
	assert.NotContains(t, store.query, "discard-this-suffix")
	assert.True(t, utf8.ValidString(store.query))

	store = &lookalikeQueryRecordingStore{}
	in.Body.Title = strings.Repeat("界", 499) + "留 discarded-title-suffix"
	in.Body.Body = "plain body"

	err = runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project")

	require.NoError(t, err)
	assert.Contains(t, store.query, "plain body")
	assert.Contains(t, store.query, "留")
	assert.NotContains(t, store.query, "discarded-title-suffix")
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

func TestRunLookalikeCheckBoundsExactTitleQuery(t *testing.T) {
	in := &api.CreateIssueRequest{ProjectID: 7}
	in.Body.Title = "Inventory page crashes on second page"
	in.Body.Body = "apples bananas cherries dates elderberries figs grapes hazelnuts"
	rows := make([]db.Issue, 20)
	for i := range rows {
		rows[i] = db.Issue{UID: fmt.Sprint(i), Title: in.Body.Title, Body: "apples"}
	}
	calls := 0
	store := &lookalikeCandidateStore{listIssues: func(params db.ListIssuesParams) ([]db.Issue, error) {
		calls++
		require.Equal(t, 20, params.Limit)
		require.True(t, params.ShortestBodyFirst)
		require.Nil(t, params.After)
		return rows, nil
	}}
	for _, row := range rows {
		store.candidates = append(store.candidates, db.SearchCandidate{Issue: row})
	}
	require.NoError(t, runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project"))
	assert.Equal(t, 1, calls)
}

func TestRunLookalikeCheckSkipsCompleteFTSPage(t *testing.T) {
	for _, tc := range []struct {
		title string
		hits  int
	}{{"Inventory page crashes", 0}, {"Inventory page crashes", 19}, {"the", 0}, {"1", 0}} {
		t.Run(fmt.Sprintf("%s/%d", tc.title, tc.hits), func(t *testing.T) {
			in := &api.CreateIssueRequest{ProjectID: 7}
			in.Body.Title = tc.title
			in.Body.Body = "apples bananas cherries dates elderberries figs grapes hazelnuts"
			store := &lookalikeCandidateStore{listIssues: func(db.ListIssuesParams) ([]db.Issue, error) {
				t.Fatal("exact-title lookup repeated a complete FTS result")
				return nil, nil
			}}
			for i := range tc.hits {
				store.candidates = append(store.candidates, db.SearchCandidate{Issue: db.Issue{
					UID: fmt.Sprint(i), Title: tc.title, Body: "apples",
				}})
			}
			require.NoError(t, runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project"))
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
