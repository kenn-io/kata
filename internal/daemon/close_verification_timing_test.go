package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type lateCloseVerdictStore struct {
	db.Storage
	issueID  int64
	target   string
	inserted bool
}

func (s *lateCloseVerdictStore) CloseIssueGuarded(ctx context.Context, p db.CloseIssueParams) (db.Issue, []db.Event, bool, error) {
	if !s.inserted {
		s.inserted = true
		if _, _, err := s.Storage.CreateComment(ctx, db.CreateCommentParams{IssueID: s.issueID, Author: "late-reviewer", Body: "A late refutation arrived after the preflight snapshot.", ReplyToUID: s.target, ReplyKind: "refute"}); err != nil {
			return db.Issue{}, nil, false, err
		}
	}
	return s.Storage.CloseIssueGuarded(ctx, p)
}

// M6: verification is a preflight snapshot; late verdicts affect live show,
// while an exact retry retains the original close decision and evidence.
func TestCloseVerificationPreflightTiming(t *testing.T) {
	wrapped := &lateCloseVerdictStore{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		wrapped.Storage = cfg.DB
		cfg.DB = wrapped
		cfg.CloseVerification = "block"
	})
	store := env.DB
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Claim", Metadata: map[string]jsontext.Value{"verify.required": jsontext.Value("true")}})
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Review"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Completion claim"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "reviewer", Body: "Independent confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	wrapped.issueID, wrapped.target = other.ID, claim.UID
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", p.ID, issue.ShortID)
	body := map[string]any{"actor": "worker", "reason": "done", "message": "Completed the requested work and checked the focused test suite.", "retry_protocol": "close-v1", "evidence": []map[string]any{{"type": "confirmed", "comment": claim.UID}}}
	closeIssue := func() (int, []byte) {
		response, raw := envDoRaw(t, env, http.MethodPost, path+"/actions/close", body, map[string]string{"Idempotency-Key": "preflight-close"})
		return response.StatusCode, raw
	}
	code, first := closeIssue()
	require.Equal(t, 200, code, string(first))
	require.NotContains(t, string(first), "verification_warnings")
	code, replay := closeIssue()
	require.Equal(t, 200, code, string(replay))
	require.Contains(t, string(replay), `"reused":true`)
	require.NotContains(t, string(replay), "verification_warnings")
	var firstResult struct {
		Event *db.Event `json:"event"`
	}
	var retryResult struct {
		OriginalEvent *db.Event `json:"original_event"`
	}
	require.NoError(t, json.Unmarshal(first, &firstResult))
	require.NoError(t, json.Unmarshal(replay, &retryResult))
	require.NotNil(t, firstResult.Event)
	require.NotNil(t, retryResult.OriginalEvent)
	require.Equal(t, firstResult.Event.Payload, retryResult.OriginalEvent.Payload)
	response, raw := envDoRaw(t, env, http.MethodGet, path, nil, nil)
	require.Equal(t, 200, response.StatusCode, string(raw))
	var out struct {
		Verification db.Verification `json:"verification"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Verification.OpenRefutations, 1)
	require.Empty(t, out.Verification.Confirmed)
	require.NotContains(t, out.Verification.Hint, "Post a completion claim")
}
