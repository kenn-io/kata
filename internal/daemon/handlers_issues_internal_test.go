package daemon

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type lookalikeQueryRecordingStore struct {
	db.Storage
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

	err := runLookalikeCheck(context.Background(), ServerConfig{DB: store}, in)

	require.NoError(t, err)
	assert.Contains(t, store.query, in.Body.Title)
	assert.Contains(t, store.query, "留")
	assert.NotContains(t, store.query, "discard-this-suffix")
	assert.True(t, utf8.ValidString(store.query))

	store = &lookalikeQueryRecordingStore{}
	in.Body.Title = strings.Repeat("界", 499) + "留 discarded-title-suffix"
	in.Body.Body = "plain body"

	err = runLookalikeCheck(context.Background(), ServerConfig{DB: store}, in)

	require.NoError(t, err)
	assert.Contains(t, store.query, "plain body")
	assert.Contains(t, store.query, "留")
	assert.NotContains(t, store.query, "discarded-title-suffix")
	assert.True(t, utf8.ValidString(store.query))
}

type lookalikeCandidateStore struct {
	db.Storage
	candidates []db.SearchCandidate
}

func (s *lookalikeCandidateStore) SearchFTSAny(_ context.Context, _ db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return s.candidates, nil
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

		err := runLookalikeCheck(context.Background(), ServerConfig{DB: store}, in)

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

		err := runLookalikeCheck(context.Background(), ServerConfig{DB: store}, in)

		require.Error(t, err)
		var apiErr *api.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 409, apiErr.Status)
		assert.Equal(t, "2 existing issues match this title", apiErr.Message)
		assert.NotContains(t, apiErr.Message, "open")
	})
}
