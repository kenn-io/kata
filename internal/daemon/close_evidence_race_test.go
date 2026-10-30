package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type closeEvidenceRaceStore struct {
	db.Storage
	beforeClose func()
}

func (s *closeEvidenceRaceStore) CloseIssueGuarded(ctx context.Context, p db.CloseIssueParams) (db.Issue, []db.Event, bool, error) {
	if s.beforeClose != nil {
		fn := s.beforeClose
		s.beforeClose = nil
		fn()
	}
	return s.Storage.CloseIssueGuarded(ctx, p)
}

// Contract: a duplicate target that moves outside the request's projects after
// validation but before the close transaction rejects and rolls back the close.
func TestCloseRejectsEvidenceTargetMovedOutOfScope(t *testing.T) {
	race := &closeEvidenceRaceStore{}
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
		race.Storage = cfg.DB
		cfg.DB = race
	})
	source, err := env.DB.CreateProject(t.Context(), "evidence-source")
	require.NoError(t, err)
	excluded, err := env.DB.CreateProject(t.Context(), "evidence-excluded")
	require.NoError(t, err)
	closing, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "closing", Author: "tester"})
	require.NoError(t, err)
	original, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "original", Author: "tester"})
	require.NoError(t, err)
	race.beforeClose = func() {
		_, err := env.DB.MoveIssueProject(context.Background(), db.MoveIssueProjectIn{IssueID: original.ID, FromProjectID: source.ID, ToProjectID: excluded.ID, IfMatchRev: original.Revision, Actor: "tester"})
		require.NoError(t, err)
	}
	body := fmt.Sprintf(`{"actor":"tester","reason":"duplicate","message":"Same report as the original issue.","evidence":[{"type":"duplicate-of","issue_ref":%q}]}`, original.ShortID)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, source.ID, closing.ShortID), strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestProjectScopeHeader, requestScopeValue(t, source.UID))
	resp, err := env.HTTP.Do(req) //nolint:gosec // G704: test server URL, not user-controlled
	require.NoError(t, err)
	raw := readClose(t, resp)
	require.Nil(t, race.beforeClose, "move must occur between validation and the close transaction")
	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
	got, err := env.DB.IssueByID(t.Context(), closing.ID)
	require.NoError(t, err)
	require.Equal(t, "open", got.Status)
}
