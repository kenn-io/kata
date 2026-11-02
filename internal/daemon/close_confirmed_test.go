package daemon_test

import (
	"encoding/json/jsontext"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCloseConfirmationRequired(t *testing.T) {
	env := testenv.New(t)
	pid := initLocalWorkspace(t, env, "example-workspace")
	id := createIssueViaHTTP(t, env, pid, "Claim")
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Completion claim"})
	require.NoError(t, err)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: id, Actor: "worker", Patch: map[string]jsontext.Value{"verify.required": jsontext.Value("true")}})
	require.NoError(t, err)
	body := map[string]any{"actor": "worker", "reason": "done", "message": "Implemented the feature and checked the focused package.", "evidence": []map[string]any{{"type": "commit", "sha": "abc1234"}}}
	closeIssue := func() (int, []byte) {
		resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), body, nil)
		return resp.StatusCode, raw
	}
	code, raw := closeIssue()
	require.Equal(t, 409, code, string(raw))
	require.Contains(t, string(raw), "confirmation_required")
	body["source"] = "tui"
	code, raw = closeIssue()
	require.Equal(t, 409, code, string(raw))
	delete(body, "source")
	body["evidence"] = []map[string]any{{"type": "confirmed", "comment": claim.UID}}
	code, raw = closeIssue()
	require.Equal(t, 400, code, string(raw))
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Self confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	code, raw = closeIssue()
	require.Equal(t, 400, code, string(raw))
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Teammate: "reviewer", Body: "Independent confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	body["evidence"] = []map[string]any{{"type": "commit", "sha": "abc1234"}}
	code, raw = closeIssue()
	require.Equal(t, 409, code, string(raw), "an uncited confirmation must not satisfy verify.required")
	body["evidence"] = []map[string]any{{"type": "confirmed", "comment": claim.UID}}
	code, raw = closeIssue()
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), "confirmed")
	require.NotContains(t, string(raw), "confirmed_claims")
	resp, raw := envDoRaw(t, env, http.MethodGet, "/api/v1/audit/closes?project_id="+strconv.FormatInt(pid, 10), nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "confirmed")
}

func TestCloseConfirmedCitationMustBeClean(t *testing.T) {
	for _, mode := range []string{"warn", "off", "block"} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CloseVerification = mode })
			pid := initLocalWorkspace(t, env, "example-workspace")
			id := createIssueViaHTTP(t, env, pid, "Claim")
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Completion claim"})
			require.NoError(t, err)
			_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
			require.NoError(t, err)
			r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Dispute", ReplyToUID: claim.UID, ReplyKind: "refute"})
			require.NoError(t, err)
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), map[string]any{"actor": "worker", "reason": "done", "message": strings.Repeat("Evidence of completed work. ", 3), "evidence": []map[string]any{{"type": "confirmed", "comment": claim.UID}}, "overrule": []string{r.UID}}, nil)
			require.Equal(t, 400, resp.StatusCode, string(raw))
			require.Contains(t, string(raw), "open refutation")
		})
	}
}

func TestCloseRefutedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, reason, kind        string
		self, contested, rebutted bool
		want                      int
	}{
		{"wontfix", "wontfix", "refuted", false, false, false, 200},
		{"duplicate", "duplicate", "refuted", false, false, false, 200},
		{"superseded", "superseded", "refuted", false, false, false, 200},
		{"self", "wontfix", "refuted", true, false, false, 400},
		{"contested", "wontfix", "refuted", false, true, false, 400},
		{"rebutted", "wontfix", "refuted", false, false, true, 400},
		{"done", "done", "refuted", false, false, false, 400},
		{"tui done", "done", "refuted", false, false, false, 400},
		{"confirmed wontfix", "wontfix", "confirmed", false, false, false, 400},
		{"audit", "audit-no-change", "refuted", false, false, false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			pid := initLocalWorkspace(t, env, "example-workspace")
			id := createIssueViaHTTP(t, env, pid, "Claim")
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Claim"})
			require.NoError(t, err)
			author := "reviewer"
			if tc.self {
				author = "worker"
			}
			r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: author, Body: "Refutation", ReplyToUID: claim.UID, ReplyKind: "refute"})
			require.NoError(t, err)
			if tc.contested {
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
				require.NoError(t, err)
			}
			if tc.rebutted {
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Rebuttal", ReplyToUID: r.UID, ReplyKind: "refute"})
				require.NoError(t, err)
			}
			evidence := []map[string]any{{"type": tc.kind, "comment": claim.UID}}
			if tc.reason == "duplicate" || tc.reason == "superseded" {
				target := createIssueViaHTTP(t, env, pid, "Target")
				i, err := env.DB.IssueByID(t.Context(), target)
				require.NoError(t, err)
				tag := "duplicate-of"
				if tc.reason == "superseded" {
					tag = "superseded-by"
				}
				evidence = append(evidence, map[string]any{"type": tag, "issue_ref": i.ShortID})
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), map[string]any{"actor": "worker", "reason": tc.reason, "source": map[bool]string{true: "tui"}[tc.name == "tui done"], "message": strings.Repeat("Reviewed this claim and reproduced the observed behavior. ", 2), "evidence": evidence}, nil)
			require.Equal(t, tc.want, resp.StatusCode, string(raw))
			if tc.reason == "done" {
				require.Contains(t, string(raw), "--overrule")
			}
		})
	}
}
