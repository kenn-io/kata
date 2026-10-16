package daemon_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func historicalMutationIssue(t *testing.T, env *testenv.Env) (db.Project, db.Project, db.Issue, string) {
	t.Helper()
	source, target, issue := seedMovePair(t, env)
	oldRef := issue.ShortID
	moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "coordinator"})
	require.NoError(t, err)
	return source, target, moved.Issue, oldRef
}

func TestHistoricalMutationClaimGate(t *testing.T) {
	for _, tc := range []struct {
		name, method, suffix string
		body                 map[string]any
		prepare              string
	}{
		{name: "edit", method: http.MethodPatch, body: map[string]any{"title": "changed"}},
		{name: "priority", suffix: "actions/priority", body: map[string]any{"priority": 1}},
		{name: "metadata", suffix: "metadata", body: map[string]any{"patch": map[string]any{"review": "pending"}}},
		{name: "assign", suffix: "actions/assign", body: map[string]any{"owner": "member"}},
		{name: "unassign", suffix: "actions/unassign"},
		{name: "claim", suffix: "actions/claim"},
		{name: "label add", suffix: "labels", body: map[string]any{"label": "review"}},
		{name: "label remove", method: http.MethodDelete, suffix: "labels/review?actor=agent"},
		{name: "close", suffix: "actions/close", body: map[string]any{"reason": "wontfix", "message": "Reviewed the proposed work and recorded why it should stop here."}},
		{name: "reopen", suffix: "actions/reopen", prepare: "close"},
		{name: "delete", suffix: "actions/delete"},
		{name: "restore", suffix: "actions/restore", prepare: "delete"},
		{name: "purge", suffix: "actions/purge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, target, issue, oldRef := historicalMutationIssue(t, env)
			var err error
			if tc.prepare == "close" {
				_, _, _, err = env.DB.CloseIssue(t.Context(), issue.ID, "wontfix", "coordinator", "Reviewed this work and recorded why it should stop here.", nil)
				require.NoError(t, err)
			}
			if tc.prepare == "delete" {
				_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), issue.ID, "coordinator")
				require.NoError(t, err)
			}
			_, err = env.DB.EnableProjectFederation(t.Context(), target.ID, "coordinator")
			require.NoError(t, err)
			_, err = env.DB.AcquireClaim(t.Context(), db.AcquireClaimParams{ProjectID: target.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: env.DB.InstanceUID(), Holder: "other"}, ClaimKind: "hard", Now: time.Now().UTC()})
			require.NoError(t, err)
			body := tc.body
			if body == nil {
				body = map[string]any{}
			}
			body["actor"] = "agent"
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			headers := map[string]string{"Authorization": "Bearer tok"}
			if tc.name == "delete" {
				headers["X-Kata-Confirm"] = "DELETE " + target.Name + "#" + issue.ShortID
			}
			if tc.name == "purge" {
				headers["X-Kata-Confirm"] = "PURGE " + target.Name + "#" + issue.ShortID
			}
			resp, raw := envDoRaw(t, env, method, issuePathRef(source.ID, oldRef, tc.suffix), body, headers)
			assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
		})
	}
}

func TestHistoricalMutationKeyedCloseRetry(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, _, issue, oldRef := historicalMutationIssue(t, env)
			path := issuePathRef(source.ID, oldRef, "actions/close")
			headers := map[string]string{"Authorization": "Bearer tok", "Idempotency-Key": "historical-close", "If-Match": fmt.Sprintf(`"rev-%d"`, issue.Revision)}
			body := map[string]any{"actor": "agent", "reason": "wontfix", "message": "Reviewed the proposed work and recorded why it should stop here.", "retry_protocol": "close-v1"}
			first, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
			require.Equal(t, http.StatusOK, first.StatusCode, string(raw))
			if deleted {
				_, _, _, err := env.DB.SoftDeleteIssue(t.Context(), issue.ID, "agent")
				require.NoError(t, err)
			}
			retry, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
			require.Equal(t, http.StatusOK, retry.StatusCode, string(raw))
			assert.Contains(t, string(raw), `"reused":true`)
		})
	}
}

func TestHistoricalMutationKeyedCommentRetry(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	source, _, _, oldRef := historicalMutationIssue(t, env)
	path := issuePathRef(source.ID, oldRef, "comments")
	headers := map[string]string{"Authorization": "Bearer tok", "Idempotency-Key": "historical-comment"}
	body := map[string]any{"actor": "agent", "body": "Review recorded."}
	first, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
	require.Equal(t, http.StatusOK, first.StatusCode, string(raw))
	retry, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
	require.Equal(t, http.StatusOK, retry.StatusCode, string(raw))
	assert.Contains(t, string(raw), `"changed":false`)
}

func TestHistoricalMutationPublishesCurrentProject(t *testing.T) {
	for _, action := range []string{"delete", "restore", "purge", "link", "unlink", "replace parent"} {
		t.Run(action, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, target, issue, oldRef := historicalMutationIssue(t, env)
			suffix := "actions/" + action
			method := http.MethodPost
			headers := map[string]string{"Authorization": "Bearer tok"}
			body := map[string]any{"actor": "agent"}
			if action == "restore" {
				_, _, _, err := env.DB.SoftDeleteIssue(t.Context(), issue.ID, "agent")
				require.NoError(t, err)
			}
			if action == "delete" {
				headers["X-Kata-Confirm"] = "DELETE " + target.Name + "#" + issue.ShortID
			}
			if action == "purge" {
				headers["X-Kata-Confirm"] = "PURGE " + target.Name + "#" + issue.ShortID
			}
			if action == "link" || action == "unlink" || action == "replace parent" {
				peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "Peer", Author: "agent"})
				require.NoError(t, err)
				suffix = "links"
				body["type"] = "related"
				body["to_ref"] = target.Name + "#" + peer.ShortID
				if action == "unlink" || action == "replace parent" {
					typ := "related"
					if action == "replace parent" {
						typ = "parent"
					}
					link, err := env.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: typ, Author: "agent"})
					require.NoError(t, err)
					if action == "unlink" {
						method = http.MethodDelete
						suffix = fmt.Sprintf("links/%d?actor=agent", link.ID)
					} else {
						replacement, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "Replacement", Author: "agent"})
						require.NoError(t, err)
						body["type"] = "parent"
						body["to_ref"] = target.Name + "#" + replacement.ShortID
						body["replace"] = true
					}
				}
			}
			sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: target.ID})
			defer sub.Unsub()
			resp, raw := envDoRaw(t, env, method, issuePathRef(source.ID, oldRef, suffix), body, headers)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			count := 1
			if action == "replace parent" {
				count = 2
			}
			for range count {
				select {
				case msg := <-sub.Ch:
					assert.Equal(t, target.ID, msg.ProjectID)
				case <-time.After(time.Second):
					t.Fatal("committed mutation did not publish to current project")
				}
			}
		})
	}
}

func TestHistoricalMutationCloseEvidenceUsesStableIdentity(t *testing.T) {
	for _, reason := range []string{"duplicate", "superseded"} {
		t.Run(reason, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, err := env.DB.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			closing, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Closing issue", Author: "agent", UID: "01ARZ3NDEKTSV4RRFFQ69AAA11", ShortIDOverride: "aaa11"})
			require.NoError(t, err)
			peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Distinct evidence target", Author: "agent", UID: "01ARZ3NDEKTSV4RRFFQ69BAA11", ShortIDOverride: "aa11"})
			require.NoError(t, err)
			moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: closing.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: closing.Revision, Actor: "agent"})
			require.NoError(t, err)
			require.Equal(t, peer.ShortID, moved.Issue.ShortID)
			typ := "duplicate-of"
			if reason == "superseded" {
				typ = "superseded-by"
			}
			body := map[string]any{"actor": "agent", "reason": reason, "message": "Reviewed the evidence and recorded the distinct replacement issue.", "evidence": []map[string]any{{"type": typ, "issue_ref": peer.ShortID}}}
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(source.ID, closing.ShortID, "actions/close"), body, map[string]string{"Authorization": "Bearer tok"})
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		})
	}
}
