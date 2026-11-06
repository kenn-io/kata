package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/pagination"
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

func TestRunLookalikeCheckExactTitlePaging(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		fts         bool
		wantCalls   int
		wantMatches int
	}{
		{"bodyless stops at twenty", "", false, 1, 20},
		{"nonmatches exhaust short page", "apples bananas cherries dates elderberries figs grapes hazelnuts", false, 2, 0},
		{"full FTS skips exact lookup", "apples", true, 0, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &api.CreateIssueRequest{ProjectID: 7}
			in.Body.Title = "Inventory page crashes on second page"
			in.Body.Body = tc.body
			stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			rows := make([]db.Issue, 21)
			for i := range rows {
				rows[i] = db.Issue{ID: int64(21 - i), UID: fmt.Sprintf("issue-%d", i), Title: in.Body.Title, Body: "apples", CreatedAt: stamp}
			}
			calls := 0
			store := &lookalikeCandidateStore{listIssues: func(params db.ListIssuesParams) ([]db.Issue, error) {
				calls++
				require.Equal(t, 20, params.Limit)
				require.True(t, params.CreatedFirst)
				require.Equal(t, int64(7), params.ProjectID)
				require.Equal(t, in.Body.Title, params.ExactTitle)
				switch calls {
				case 1:
					require.Nil(t, params.After)
					return rows[:20], nil
				case 2:
					require.Equal(t, &pagination.Position{CreatedAt: stamp, ID: 2}, params.After)
					return rows[20:], nil
				default:
					t.Fatal("exact lookup continued after exhaustion")
					return nil, nil
				}
			}}
			if tc.fts {
				for _, issue := range rows[:20] {
					store.candidates = append(store.candidates, db.SearchCandidate{Issue: issue})
				}
			}
			err := runLookalikeCheck(t.Context(), ServerConfig{DB: store}, in, "example-project")
			if tc.wantMatches == 0 {
				require.NoError(t, err)
			} else {
				var apiErr *api.APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, "duplicate_candidates", apiErr.Code)
				assert.Len(t, apiErr.Data["candidates"], tc.wantMatches)
			}
			assert.Equal(t, tc.wantCalls, calls)
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
