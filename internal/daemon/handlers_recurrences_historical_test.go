package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestPostRecurrenceRejectsHistoricalInitialIssueFromAnotherProject(t *testing.T) {
	for _, kind := range []string{"historical short ID", "ULID"} {
		t.Run(kind, func(t *testing.T) {
			env := testenv.New(t)
			source, target, issue := seedMovePair(t, env)
			_, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			ref := issue.ShortID
			if kind == "ULID" {
				ref = issue.UID
			}
			response, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/recurrences", source.ID), map[string]any{
				"actor": "worker", "initial_issue_ref": ref, "rrule": "FREQ=WEEKLY", "dtstart": "2026-10-12", "timezone": "UTC", "template": map[string]string{"title": "Weekly review"},
			}, nil)
			assertAPIError(t, response.StatusCode, raw, http.StatusBadRequest, "validation")
			require.Contains(t, string(raw), "initial_issue_ref")
			for _, project := range []db.Project{source, target} {
				recurrences, err := env.DB.ListRecurrencesByProject(t.Context(), project.ID)
				require.NoError(t, err)
				require.Empty(t, recurrences)
			}
			stored, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, stored.ProjectID)
			require.Nil(t, stored.RecurrenceID)
		})
	}
}

type recurrenceInitialIssueMoveStore struct {
	db.Storage
	beforeCreate func()
}

func (s *recurrenceInitialIssueMoveStore) CreateRecurrenceForIssue(ctx context.Context, input db.CreateRecurrenceForIssueIn) (db.CreateRecurrenceForIssueOut, error) {
	if s.beforeCreate != nil {
		move := s.beforeCreate
		s.beforeCreate = nil
		move()
	}
	return s.Storage.CreateRecurrenceForIssue(ctx, input)
}

func TestPostRecurrenceInitialIssueMovingBeforeWriteReturnsNotFound(t *testing.T) {
	var wrapped *recurrenceInitialIssueMoveStore
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		wrapped = &recurrenceInitialIssueMoveStore{Storage: cfg.DB}
		cfg.DB = wrapped
	})
	source, target, issue := seedMovePair(t, env)
	wrapped.beforeCreate = func() {
		_, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
	}
	response, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/recurrences", source.ID), map[string]any{
		"actor": "worker", "initial_issue_ref": issue.ShortID, "rrule": "FREQ=WEEKLY", "dtstart": "2026-10-12", "timezone": "UTC", "template": map[string]string{"title": "Weekly review"},
	}, nil)
	assertAPIError(t, response.StatusCode, raw, http.StatusNotFound, "issue_not_found")
	recurrences, err := env.DB.ListRecurrencesByProject(t.Context(), source.ID)
	require.NoError(t, err)
	require.Empty(t, recurrences)
	stored, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, stored.ProjectID)
	require.Nil(t, stored.RecurrenceID)
}
