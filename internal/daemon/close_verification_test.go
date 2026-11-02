package daemon_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCloseVerificationGuard(t *testing.T) {
	for _, mode := range []string{"block", "warn", "off", ""} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CloseVerification = mode })
			pid := initLocalWorkspace(t, env, "example-workspace")
			id := createIssueViaHTTP(t, env, pid, "Claim")
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Claim"})
			require.NoError(t, err)
			r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "The proposed implementation does not meet the contract.", ReplyToUID: claim.UID, ReplyKind: "refute"})
			require.NoError(t, err)
			body := map[string]any{"actor": "worker", "reason": "done", "message": "Implemented the feature and checked the focused package.", "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}}}
			closeIssue := func() (int, []byte) {
				resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), body, nil)
				return resp.StatusCode, raw
			}
			code, raw := closeIssue()
			if mode == "block" {
				require.Equal(t, 409, code, string(raw))
				require.Contains(t, string(raw), "open_refutations")
				body["overrule"] = []string{"c:unknown"}
				code, raw = closeIssue()
				require.Equal(t, 400, code, string(raw))
				body["overrule"] = []string{r.UID}
				code, raw = closeIssue()
				require.Equal(t, 200, code, string(raw))
				require.Contains(t, string(raw), "overruled_refutations")
			} else {
				require.Equal(t, 200, code, string(raw))
				if mode == "off" {
					require.NotContains(t, string(raw), "verification_warnings")
				} else {
					require.Contains(t, string(raw), "verification_warnings")
					require.Contains(t, string(raw), "warnings")
				}
			}
		})
	}
}

func TestCloseVerificationResolution(t *testing.T) {
	for _, resolution := range []string{"supersede-claim", "supersede-refutation", "rebuttal", "conflict", "refuted-rebuttal"} {
		t.Run(resolution, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CloseVerification = "block" })
			pid := initLocalWorkspace(t, env, "example-workspace")
			id := createIssueViaHTTP(t, env, pid, "Claim")
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Claim"})
			require.NoError(t, err)
			r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Disputed claim", ReplyToUID: claim.UID, ReplyKind: "refute"})
			require.NoError(t, err)
			kind, target := "supersede", claim.UID
			switch resolution {
			case "supersede-refutation":
				target = r.UID
			case "rebuttal", "refuted-rebuttal":
				kind, target = "refute", r.UID
			case "conflict":
				kind = "confirm"
			}
			b, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Teammate: "other", Body: strings.Repeat("Evidence ", 8), ReplyToUID: target, ReplyKind: kind})
			require.NoError(t, err)
			if resolution == "refuted-rebuttal" {
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Reopened", ReplyToUID: b.UID, ReplyKind: "refute"})
				require.NoError(t, err)
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), map[string]any{"actor": "worker", "reason": "done", "message": "Implemented the feature and checked the focused package.", "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}}}, nil)
			want := 200
			if resolution == "conflict" || resolution == "refuted-rebuttal" {
				want = 409
			}
			require.Equal(t, want, resp.StatusCode, string(raw))
		})
	}
}
