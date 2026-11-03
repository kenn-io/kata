package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestShowVerification(t *testing.T) {
	env := testenv.New(t)
	pid := initLocalWorkspace(t, env, "example-workspace")
	id := createIssueViaHTTP(t, env, pid, "Claim")
	show := func() *db.Verification {
		resp, raw := envDoRaw(t, env, http.MethodGet, issuePath(pid, id, ""), nil, nil)
		require.Equal(t, 200, resp.StatusCode, string(raw))
		var out struct {
			Verification *db.Verification `json:"verification"`
		}
		require.NoError(t, json.Unmarshal(raw, &out))
		if out.Verification == nil {
			var fields map[string]any
			require.NoError(t, json.Unmarshal(raw, &fields))
			require.NotContains(t, fields, "verification")
		}
		return out.Verification
	}
	require.Nil(t, show(), "omit empty summaries")
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Completion claim"})
	require.NoError(t, err)
	other := createIssueViaHTTP(t, env, pid, "Review")
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: other, Author: "worker", Teammate: "reviewer", Body: "Independent confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	v := show()
	require.NotNil(t, v)
	require.Len(t, v.Confirmed, 1)
	require.Empty(t, v.OpenRefutations)
	r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: other, Author: "reviewer", Body: "Disputed", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	v = show()
	require.Len(t, v.OpenRefutations, 1)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: other, Author: "worker", Body: "Rebuttal", ReplyToUID: r.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	v = show()
	require.Empty(t, v.OpenRefutations)
	require.Len(t, v.Confirmed, 1, "show must read beyond immediate neighbors")
}
