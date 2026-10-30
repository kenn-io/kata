package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
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
			body := map[string]any{"actor": "agent", "reason": reason, "message": "Reviewed the evidence and recorded the distinct replacement issue.", "evidence": []map[string]any{{"type": typ, "issue_ref": peer.ShortID}}, "retry_protocol": "close-v1"}
			path := issuePathRef(source.ID, closing.ShortID, "actions/close")
			headers := map[string]string{"Authorization": "Bearer tok", "Idempotency-Key": "historical-evidence"}
			resp, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: target.ID, IssueUID: closing.UID, Types: []string{"issue.closed"}, Limit: 10})
			require.NoError(t, err)
			require.Len(t, events, 1)
			var payload struct {
				Evidence []db.Evidence `json:"evidence"`
			}
			require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
			require.Len(t, payload.Evidence, 1)
			require.Equal(t, peer.UID, payload.Evidence[0].IssueRef, "persist the validated target, independent of the event project")
			retry, replay := envDoRaw(t, env, http.MethodPost, path, body, headers)
			require.Equal(t, http.StatusOK, retry.StatusCode, string(replay))
			var reused struct {
				Reused        bool     `json:"reused"`
				OriginalEvent db.Event `json:"original_event"`
			}
			require.NoError(t, json.Unmarshal(replay, &reused))
			require.True(t, reused.Reused, "fingerprint the original request rather than normalized evidence")
			require.Equal(t, events[0].UID, reused.OriginalEvent.UID)
			require.Equal(t, events[0].Payload, reused.OriginalEvent.Payload)
		})
	}
}

func TestHistoricalMutationKeyedCloseRetryAcrossMultipleMoves(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			source, intermediate, issue, _ := historicalMutationIssue(t, env)
			headers := map[string]string{"Authorization": "Bearer tok", "Idempotency-Key": "multi-hop-close", "If-Match": fmt.Sprintf(`"rev-%d"`, issue.Revision)}
			body := map[string]any{"actor": "agent", "reason": "wontfix", "message": "Reviewed the proposed work and recorded why it should stop here.", "retry_protocol": "close-v1"}
			path := issuePathRef(source.ID, issue.UID, "actions/close")
			first, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
			require.Equal(t, http.StatusOK, first.StatusCode, string(raw))
			var original struct {
				Event db.Event `json:"event"`
			}
			require.NoError(t, json.Unmarshal(raw, &original))
			require.Equal(t, intermediate.ID, original.Event.ProjectID)
			current, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "final-project")
			require.NoError(t, err)
			_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: intermediate.ID, ToProjectID: target.ID, IfMatchRev: current.Revision, Actor: "coordinator"})
			require.NoError(t, err)
			if deleted {
				_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), issue.ID, "agent")
				require.NoError(t, err)
			}
			for _, route := range []int64{source.ID, target.ID} {
				retry, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(route, issue.UID, "actions/close"), body, headers)
				require.Equal(t, http.StatusOK, retry.StatusCode, string(raw))
				var replay struct {
					Reused        bool     `json:"reused"`
					OriginalEvent db.Event `json:"original_event"`
					Issue         db.Issue `json:"issue"`
				}
				require.NoError(t, json.Unmarshal(raw, &replay))
				assert.True(t, replay.Reused)
				assert.Equal(t, original.Event.UID, replay.OriginalEvent.UID)
				assert.Equal(t, target.ID, replay.Issue.ProjectID)
			}
			team, _, err := env.DB.CreateTeam(t.Context(), "restricted-team", "admin")
			require.NoError(t, err)
			_, _, err = env.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
				ProjectUID: target.UID, Visibility: "teams", TeamUIDs: []string{team.UID},
			}, "admin")
			require.NoError(t, err)
			_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
				PlaintextToken: "restricted-agent-token", Actor: "agent", AdminActor: "admin",
			})
			require.NoError(t, err)
			server := daemon.NewServer(daemon.ServerConfig{
				DB: env.DB, StartedAt: time.Now(), Auth: config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true},
			})
			defer func() { _ = server.Close() }()
			hostServer := httptest.NewServer(server.Handler())
			defer hostServer.Close()
			headers["Authorization"] = "Bearer restricted-agent-token"
			retry := postWithHeader(t, hostServer, path, headers, body)
			assertAPIError(t, retry.status, retry.body, http.StatusNotFound, "issue_not_found")
		})
	}
}

func TestHistoricalMutationConcurrentCloseKeyCollision(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	source, target, first, _ := historicalMutationIssue(t, env)
	otherSource, err := env.DB.CreateProject(t.Context(), "other-source")
	require.NoError(t, err)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: otherSource.ID, Title: "Second historical issue", Author: "agent"})
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: second.ID, FromProjectID: otherSource.ID, ToProjectID: target.ID, IfMatchRev: second.Revision, Actor: "coordinator"})
	require.NoError(t, err)
	type outcome struct {
		status int
		raw    []byte
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, path := range []string{issuePathRef(source.ID, first.UID, "actions/close"), issuePathRef(otherSource.ID, second.UID, "actions/close")} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+path, strings.NewReader(`{"actor":"agent","reason":"wontfix","message":"Reviewed the proposed work and recorded why it should stop here.","retry_protocol":"close-v1"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Idempotency-Key", "concurrent-close")
		req.Header.Set("Content-Type", "application/json")
		go func() {
			<-start
			resp, err := env.HTTP.Do(req)
			if err != nil {
				results <- outcome{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			results <- outcome{status: resp.StatusCode, raw: raw, err: err}
		}()
	}
	close(start)
	statuses := []int{}
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		statuses = append(statuses, result.status)
		if result.status != http.StatusOK {
			assertAPIError(t, result.status, result.raw, http.StatusConflict, "idempotency_mismatch")
		}
	}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
	closed := 0
	for _, id := range []int64{first.ID, second.ID} {
		issue, err := env.DB.IssueByID(t.Context(), id)
		require.NoError(t, err)
		if issue.Status == "closed" {
			closed++
		}
	}
	assert.Equal(t, 1, closed)
}

// Contract: replaying a close receipt after the issue moved never returns the
// original event when its project is outside the request's projects.
func TestHistoricalCloseReplayOmitsEventFromExcludedProject(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	source, err := env.DB.CreateProject(t.Context(), "replay-source")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "replay-target")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Replayed close", Author: "agent"})
	require.NoError(t, err)
	body := `{"actor":"agent","reason":"wontfix","message":"Reviewed the proposed work and recorded why it should stop here.","retry_protocol":"close-v1"}`
	send := func(projectID int64, scope string) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+issuePathRef(projectID, issue.UID, "actions/close"), strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Idempotency-Key", "replayed-close")
		req.Header.Set("Content-Type", "application/json")
		if scope != "" {
			req.Header.Set(requestProjectScopeHeader, requestScopeValue(t, scope))
		}
		resp, err := env.HTTP.Do(req) //nolint:gosec // G704: test server URL, not user-controlled
		require.NoError(t, err)
		return resp.StatusCode, readClose(t, resp)
	}
	status, raw := send(source.ID, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	closed, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: closed.Revision, Actor: "coordinator"})
	require.NoError(t, err)
	status, raw = send(target.ID, target.UID)
	require.Equal(t, http.StatusOK, status, string(raw))
	var replay struct {
		Reused        bool            `json:"reused"`
		OriginalEvent json.RawMessage `json:"original_event"`
	}
	require.NoError(t, json.Unmarshal(raw, &replay))
	require.True(t, replay.Reused, string(raw))
	require.True(t, len(replay.OriginalEvent) == 0 || string(replay.OriginalEvent) == "null", "original event from an excluded project: %s", raw)
	require.NotContains(t, string(raw), source.UID)
	require.NotContains(t, string(raw), source.Name)
}
