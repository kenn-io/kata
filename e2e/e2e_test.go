package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestSmoke_FullLifecycle(t *testing.T) {
	env := testenv.New(t)
	dir := initRepo(t, "https://github.com/wesm/system.git")

	// 1. init via HTTP.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects",
		map[string]any{"start_path": dir}))

	// 2. resolve project id.
	pid := resolvePID(t, env.HTTP, env.URL, dir)
	pidStr := strconv.FormatInt(pid, 10)

	// 3. create issue. Capture short_id from the response so subsequent
	// calls address the issue by its short_id (the {ref} URL component
	// rejects bare numeric ids).
	first := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]any{"actor": "agent", "title": "first", "body": "details"})

	// 4. list — body must contain the issue title.
	listBody := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues")
	assert.Contains(t, listBody, `"title":"first"`)

	// 5. comment.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+first.ShortID+"/comments",
		map[string]any{"actor": "agent", "body": "looks good"}))

	// 6. close — and verify the issue is actually closed before we move on.
	// A 200 from the action endpoint is necessary but not sufficient; a buggy
	// handler could no-op while still answering 200.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+first.ShortID+"/actions/close",
		map[string]any{
			"actor":    "agent",
			"reason":   "done",
			"message":  "Closed after verifying the fix end to end across the affected code paths.",
			"evidence": []map[string]any{{"type": "commit", "sha": "abc1234"}},
		}))
	closedBody := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+first.ShortID)
	assert.Contains(t, closedBody, `"status":"closed"`, "issue must be closed before reopen")

	// 7. reopen.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+first.ShortID+"/actions/reopen",
		map[string]any{"actor": "agent"}))

	// 8. show with comments — issue is open again, comment from step 5 is preserved.
	showBody := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+first.ShortID)
	assert.Contains(t, showBody, `"body":"looks good"`)
	assert.Contains(t, showBody, `"status":"open"`)
}

// TestSmoke_Plan2Lifecycle exercises the Plan 2 verbs end-to-end via HTTP:
// link, label, assign, ready (with blocked filtering), unassign, label rm,
// and unlink — all on a real daemon over a loopback listener.
func TestSmoke_Plan2Lifecycle(t *testing.T) {
	env := testenv.New(t)
	dir := initRepo(t, "https://github.com/wesm/system.git")

	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects",
		map[string]any{"start_path": dir}))
	pid := resolvePID(t, env.HTTP, env.URL, dir)
	pidStr := strconv.FormatInt(pid, 10)

	parent := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]any{"actor": "agent", "title": "parent"})
	child := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]any{"actor": "agent", "title": "child"})

	// Hierarchy: child has parent.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+child.ShortID+"/links",
		map[string]any{"actor": "agent", "type": "parent", "to_ref": parent.ShortID}))

	// Label child as bug.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+child.ShortID+"/labels",
		map[string]any{"actor": "agent", "label": "bug"}))

	// Assign child to alice.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+child.ShortID+"/actions/assign",
		map[string]any{"actor": "agent", "owner": "alice"}))

	// parent links don't gate ready — only blocks links do. Both issues are
	// ready right now.
	readyBody := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/ready")
	assert.Contains(t, readyBody, `"title":"parent"`)
	assert.Contains(t, readyBody, `"title":"child"`)

	// Now make parent block child explicitly. child must drop out of ready.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+parent.ShortID+"/links",
		map[string]any{"actor": "agent", "type": "blocks", "to_ref": child.ShortID}))
	readyBody = getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/ready")
	assert.Contains(t, readyBody, `"title":"parent"`)
	assert.NotContains(t, readyBody, `"title":"child"`,
		"child must be filtered while parent (blocker) is open")

	// Look up the blocks-link id so we can DELETE it after unassign.
	parentShow := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+parent.ShortID)
	var parentBody struct {
		Links []struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"links"`
	}
	require.NoError(t, json.Unmarshal([]byte(parentShow), &parentBody))
	var blocksLinkID int64
	for _, l := range parentBody.Links {
		if l.Type == "blocks" {
			blocksLinkID = l.ID
			break
		}
	}
	require.NotZero(t, blocksLinkID, "blocks link must be present on parent before unlink")

	// Unassign + remove label + unlink to verify the reverse paths.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+child.ShortID+"/actions/unassign",
		map[string]any{"actor": "agent"}))
	deleteWith(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+child.ShortID+"/labels/bug?actor=agent")
	deleteWith(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+parent.ShortID+"/links/"+
		strconv.FormatInt(blocksLinkID, 10)+"?actor=agent")

	// show child must reflect the post-state: owner cleared, bug label gone,
	// parent link still present. Decode the response so the owner check is
	// exact — a substring miss could pass if the handler wrote a different
	// owner string (or "") instead of clearing the column.
	showBody := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+child.ShortID)
	var post struct {
		Issue struct {
			Owner *string `json:"owner"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(showBody), &post),
		"show response must decode")
	assert.Nil(t, post.Issue.Owner, "owner must be nil after unassign")
	assert.NotContains(t, showBody, `"label":"bug"`, "bug label must be gone from child")
	assert.Contains(t, showBody, `"parent"`, "parent link must still be present on child")

	// And the blocks link must be gone — child is ready again.
	finalReadyBody := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/ready")
	assert.Contains(t, finalReadyBody, `"title":"child"`,
		"child must be ready again after the blocks link is removed")
}

// deleteWith issues a DELETE through the bounded testenv client and asserts
// the response is 200.
func deleteWith(t *testing.T, client *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	require.NoError(t, err)
	resp, err := client.Do(req) //nolint:gosec // G704: test-only loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := drain(t, resp)
	require.Equalf(t, 200, resp.StatusCode, "DELETE %s → %d: %s", url, resp.StatusCode, body)
}

// helpers

func initRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", dir, "init", "--quiet").Run())                 //nolint:gosec // G204: test-controlled args
	require.NoError(t, exec.Command("git", "-C", dir, "remote", "add", "origin", origin).Run()) //nolint:gosec // G204: test-controlled origin
	return dir
}

// postJSON sends a request through the bounded testenv client so a hung
// handler fails the test fast instead of stalling on the global timeout.
func postJSON(t *testing.T, client *http.Client, url string, body any) *http.Response {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	deadline := time.Now().Add(5 * time.Second)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(bs))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req) //nolint:gosec // G704: test-only loopback, caller-controlled URL
		require.NoError(t, err)
		if resp.StatusCode != http.StatusInternalServerError {
			return resp
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if !isSQLiteBusyMessage(string(raw)) || time.Now().After(deadline) {
			return resp
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
}

func isSQLiteBusyMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

// getBody runs a GET via the bounded testenv client and returns the response
// body as a string. The body is fully drained so the connection can be reused.
func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := client.Do(req) //nolint:gosec // G704: test-only loopback, caller-controlled URL
	require.NoError(t, err)
	body := drain(t, resp)
	require.Equal(t, 200, resp.StatusCode, body)
	return body
}

func requireOK(t *testing.T, resp *http.Response) {
	t.Helper()
	body := drain(t, resp)
	require.Equalf(t, 200, resp.StatusCode, "body: %s", body)
}

// createdIssue is the minimal view of an issue returned from a POST
// /issues create call. ShortID is what subsequent {ref} URL components
// must use (numeric ids no longer resolve).
type createdIssue struct {
	ShortID string
	UID     string
}

// createIssue posts a create-issue body and returns the new issue's
// short_id and uid, asserting a 200.
func createIssue(t *testing.T, client *http.Client, url string, body any) createdIssue {
	t.Helper()
	resp := postJSON(t, client, url, body)
	bs := drain(t, resp)
	require.Equalf(t, 200, resp.StatusCode, "create issue body: %s", bs)
	return decodeMutationIssue(t, []byte(bs))
}

// decodeMutationIssue extracts the (short_id, uid) pair from a
// MutationResponse-shaped body.
func decodeMutationIssue(t *testing.T, body []byte) createdIssue {
	t.Helper()
	var parsed struct {
		Issue struct {
			ShortID string `json:"short_id"`
			UID     string `json:"uid"`
		} `json:"issue"`
	}
	require.NoErrorf(t, json.Unmarshal(body, &parsed), "decode mutation body: %s", body)
	require.NotEmptyf(t, parsed.Issue.ShortID, "short_id missing from response: %s", body)
	return createdIssue{ShortID: parsed.Issue.ShortID, UID: parsed.Issue.UID}
}

func resolvePID(t *testing.T, client *http.Client, baseURL, dir string) int64 {
	t.Helper()
	resp := postJSON(t, client, baseURL+"/api/v1/projects/resolve", map[string]any{"start_path": dir})
	body := drain(t, resp)
	require.Equal(t, 200, resp.StatusCode, body)
	var b struct {
		Project struct{ ID int64 } `json:"project"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &b), body)
	return b.Project.ID
}

// drain reads and closes the response body, returning the contents as a
// string. Use this on every response so the http.Client's connection pool can
// be reused.
func drain(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(bs)
}

// smokeResp captures status + drained body so callers can read the body
// multiple times without stream-position concerns.
type smokeResp struct {
	status int
	body   []byte
}

// postWithHeaderHTTP is postJSON + extra request headers, draining the body
// up front so callers can inspect both status and body. Used by the Plan 3
// lifecycle smoke for Idempotency-Key and X-Kata-Confirm.
func postWithHeaderHTTP(t *testing.T, client *http.Client, url string,
	headers map[string]string, body any) smokeResp {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(bs))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) //nolint:gosec // G704: test-only loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return smokeResp{status: resp.StatusCode, body: out}
}

// getStatusBodyHTTP runs a GET and returns the response + drained body
// without asserting a 2xx status, so callers can verify 404s.
func getStatusBodyHTTP(t *testing.T, client *http.Client, url string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := client.Do(req) //nolint:gosec // G704: test-only loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, bs
}

// TestSmoke_Plan3Lifecycle exercises the search → idempotent create →
// look-alike block → force-new bypass → soft-delete → restore → purge end
// to end against a real testenv daemon. A single failure here surfaces any
// regression that crosses Plan 3's seam between DB, daemon, and CLI layers.
func TestSmoke_Plan3Lifecycle(t *testing.T) {
	env := testenv.New(t)
	dir := initRepo(t, "https://github.com/wesm/system.git")
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects",
		map[string]any{"start_path": dir}))
	pid := resolvePID(t, env.HTTP, env.URL, dir)
	pidStr := strconv.FormatInt(pid, 10)

	// 1. create with idempotency key.
	first := postWithHeaderHTTP(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]string{"Idempotency-Key": "smoke-K1"},
		map[string]any{"actor": "agent", "title": "fix login crash on Safari", "body": "stack trace"})
	require.Equalf(t, 200, first.status, "first create: %s", string(first.body))
	// Reused is `omitempty`; absent on not-reused responses.
	assert.NotContains(t, string(first.body), `"reused"`)
	firstIssue := decodeMutationIssue(t, first.body)

	// 2. repeat with the same key — reuse, same fingerprint.
	second := postWithHeaderHTTP(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]string{"Idempotency-Key": "smoke-K1"},
		map[string]any{"actor": "agent", "title": "fix login crash on Safari", "body": "stack trace"})
	require.Equal(t, 200, second.status, string(second.body))
	assert.Contains(t, string(second.body), `"reused":true`)
	assert.Contains(t, string(second.body), `"original_event"`)

	// 3. search picks up the issue.
	bs := getBody(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/search?q=login")
	assert.Contains(t, bs, `"title":"fix login crash on Safari"`)

	// 4. look-alike soft-block on a near-identical title.
	resp := postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]any{"actor": "agent", "title": "fix login crash Safari",
			"body": "stack trace"})
	body := drain(t, resp)
	require.Equal(t, 409, resp.StatusCode, body)
	assert.Contains(t, body, `"duplicate_candidates"`)

	// 5. force_new bypasses.
	resp = postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues",
		map[string]any{"actor": "agent", "title": "fix login crash Safari",
			"body": "stack trace", "force_new": true})
	body = drain(t, resp)
	require.Equal(t, 200, resp.StatusCode, body)
	secondIssue := decodeMutationIssue(t, []byte(body))

	// 6. soft-delete the first issue with confirm header.
	projectName := "system"
	delResp := postWithHeaderHTTP(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+firstIssue.ShortID+"/actions/delete",
		map[string]string{"X-Kata-Confirm": "DELETE " + projectName + "#" + firstIssue.ShortID},
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, delResp.status, string(delResp.body))
	assert.Contains(t, string(delResp.body), `"issue.soft_deleted"`)

	// 7. show without include_deleted now 404s.
	showResp, _ := getStatusBodyHTTP(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+firstIssue.ShortID)
	assert.Equal(t, 404, showResp.StatusCode)

	// 8. restore brings it back.
	resp = postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+firstIssue.ShortID+"/actions/restore",
		map[string]any{"actor": "agent"})
	body = drain(t, resp)
	require.Equal(t, 200, resp.StatusCode, body)

	// 8b. confirm restore actually unstuck deleted_at — show without
	// include_deleted must succeed now. A regression that reported success
	// while leaving the issue soft-deleted would 404 here (the show handler
	// hides soft-deleted rows by default), so this catches the silent-bug
	// case step 8 alone would miss. The Issue DTO uses `omitempty` on the
	// deleted_at pointer, so a successful 200 with the field absent
	// confirms the column was cleared.
	postRestore, postRestoreBody := getStatusBodyHTTP(t, env.HTTP,
		env.URL+"/api/v1/projects/"+pidStr+"/issues/"+firstIssue.ShortID)
	require.Equal(t, 200, postRestore.StatusCode, string(postRestoreBody))
	assert.NotContains(t, string(postRestoreBody), `"deleted_at"`,
		"restore must clear deleted_at; the field is omitempty so it should be absent")

	// 9. purge the second issue (irreversible). Verify purge_log row in the response.
	purgeResp := postWithHeaderHTTP(t, env.HTTP, env.URL+"/api/v1/projects/"+pidStr+"/issues/"+secondIssue.ShortID+"/actions/purge",
		map[string]string{"X-Kata-Confirm": "PURGE " + projectName + "#" + secondIssue.ShortID},
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, purgeResp.status, string(purgeResp.body))
	assert.Contains(t, string(purgeResp.body), `"purge_log"`)
	assert.Contains(t, string(purgeResp.body), `"purge_reset_after_event_id"`)
}

// TestSmoke_Plan4Events exercises Plan 4 end-to-end via HTTP:
//  1. boot daemon, init two projects A/B, create one issue each
//  2. capture baseline cursor for project A so setup events are absorbed
//  3. open SSE consumer for project A with cursor = baseline_A
//  4. mutate project A (comment, label, assign), and project B (comment)
//  5. SSE consumer sees three frames (A's events) and no project B events
//  6. polling client returns three events from baseline_A; subsequent poll empty
//  7. parent --replace path emits issue.unlinked + issue.linked + issue.created
//  8. purge issue 1 in project A → SSE sees sync.reset_required, stream closes
//  9. polling client (with stale cursor) gets reset_required:true
//  10. reconnect SSE with reset cursor; resume cleanly with no further events
func TestSmoke_Plan4Events(t *testing.T) {
	env := testenv.New(t)
	dirA := initRepo(t, "https://github.com/wesm/plan4-a.git")
	dirB := initRepo(t, "https://github.com/wesm/plan4-b.git")

	// 1. init both projects.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects",
		map[string]any{"start_path": dirA}))
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects",
		map[string]any{"start_path": dirB}))

	pidA := resolvePID(t, env.HTTP, env.URL, dirA)
	pidB := resolvePID(t, env.HTTP, env.URL, dirB)
	pidAStr := strconv.FormatInt(pidA, 10)
	pidBStr := strconv.FormatInt(pidB, 10)

	// One issue in each.
	firstA := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/issues",
		map[string]any{"actor": "agent", "title": "first-A"})
	firstB := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidBStr+"/issues",
		map[string]any{"actor": "agent", "title": "first-B"})

	// 2. baseline cursor for project A.
	baselineA := pollNextAfterID(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/events?after_id=0")
	require.Greater(t, baselineA, int64(0))

	// 3. open SSE consumer at baseline_A.
	sseResp := openSSEAt(t, env.HTTP, env.URL+"/api/v1/events/stream?project_id="+pidAStr+
		"&after_id="+strconv.FormatInt(baselineA, 10))
	defer func() { _ = sseResp.Body.Close() }()
	framer := newSmokeFramer(sseResp.Body)

	// 4. mutate.
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/issues/"+firstA.ShortID+"/comments",
		map[string]any{"actor": "agent", "body": "comment-1"}))
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/issues/"+firstA.ShortID+"/labels",
		map[string]any{"actor": "agent", "label": "bug"}))
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/issues/"+firstA.ShortID+"/actions/assign",
		map[string]any{"actor": "agent", "owner": "claude"}))
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidBStr+"/issues/"+firstB.ShortID+"/comments",
		map[string]any{"actor": "agent", "body": "comment-B"}))

	// 5. SSE sees exactly the three project-A frames.
	frames := framer.NextN(t, 3, 2*time.Second)
	require.Len(t, frames, 3)
	assert.Equal(t, "issue.commented", frames[0].event)
	assert.Equal(t, "issue.labeled", frames[1].event)
	assert.Equal(t, "issue.assigned", frames[2].event)

	// 6. polling client returns the same three; subsequent poll is empty.
	resp, err := env.HTTP.Get(env.URL + "/api/v1/projects/" + pidAStr +
		"/events?after_id=" + strconv.FormatInt(baselineA, 10))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var pollBody struct {
		ResetRequired bool `json:"reset_required"`
		Events        []struct {
			Type string `json:"type"`
		} `json:"events"`
		NextAfterID int64 `json:"next_after_id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pollBody))
	require.Len(t, pollBody.Events, 3)
	require.Greater(t, pollBody.NextAfterID, baselineA)

	resp2, err := env.HTTP.Get(env.URL + "/api/v1/projects/" + pidAStr +
		"/events?after_id=" + strconv.FormatInt(pollBody.NextAfterID, 10))
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	var poll2 struct {
		Events      []struct{} `json:"events"`
		NextAfterID int64      `json:"next_after_id"`
	}
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&poll2))
	assert.Len(t, poll2.Events, 0)
	assert.Equal(t, pollBody.NextAfterID, poll2.NextAfterID)

	// 7. parent --replace: create a child with parent firstA, then re-link to a fresh new-parent with replace.
	childA := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/issues",
		map[string]any{"actor": "agent", "title": "child",
			"links": []map[string]any{{"type": "parent", "to_ref": firstA.ShortID}}})
	newParentA := createIssue(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/issues",
		map[string]any{"actor": "agent", "title": "new-parent"})
	requireOK(t, postJSON(t, env.HTTP, env.URL+"/api/v1/projects/"+pidAStr+"/issues/"+childA.ShortID+"/links",
		map[string]any{"actor": "agent", "type": "parent", "to_ref": newParentA.ShortID, "replace": true}))

	// New SSE frames: issue.created (childA), issue.created (newParentA),
	// then issue.unlinked + issue.linked from the replace.
	moreFrames := framer.NextN(t, 4, 2*time.Second)
	require.Len(t, moreFrames, 4)
	assert.Equal(t, "issue.created", moreFrames[0].event)
	assert.Equal(t, "issue.created", moreFrames[1].event)
	assert.Equal(t, "issue.unlinked", moreFrames[2].event, "replace must emit unlinked first")
	assert.Equal(t, "issue.linked", moreFrames[3].event, "then linked")

	// 8. purge firstA (issue 1 in project A).
	purgeURL := env.URL + "/api/v1/projects/" + pidAStr + "/issues/" + firstA.ShortID + "/actions/purge"
	pReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, purgeURL,
		strings.NewReader(`{"actor":"agent"}`))
	require.NoError(t, err)
	pReq.Header.Set("Content-Type", "application/json")
	pReq.Header.Set("X-Kata-Confirm", "PURGE plan4-a#"+firstA.ShortID)
	pResp, err := env.HTTP.Do(pReq) //nolint:gosec // G704: test-only loopback
	require.NoError(t, err)
	_ = pResp.Body.Close()
	require.Equal(t, 200, pResp.StatusCode)

	resetFrames := framer.NextN(t, 1, 2*time.Second)
	require.Len(t, resetFrames, 1)
	assert.Equal(t, "sync.reset_required", resetFrames[0].event)
	resetID, err := strconv.ParseInt(resetFrames[0].id, 10, 64)
	require.NoError(t, err)
	require.Greater(t, resetID, int64(0))

	// 9. polling client with stale cursor.
	resp3, err := env.HTTP.Get(env.URL + "/api/v1/projects/" + pidAStr +
		"/events?after_id=" + strconv.FormatInt(baselineA, 10))
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	var poll3 struct {
		ResetRequired bool  `json:"reset_required"`
		ResetAfterID  int64 `json:"reset_after_id"`
		NextAfterID   int64 `json:"next_after_id"`
	}
	require.NoError(t, json.NewDecoder(resp3.Body).Decode(&poll3))
	assert.True(t, poll3.ResetRequired)
	assert.Equal(t, resetID, poll3.ResetAfterID)
	assert.Equal(t, resetID, poll3.NextAfterID)

	// 10. reconnect SSE with reset cursor; should be clean (no further frames).
	sseResp2 := openSSEAt(t, env.HTTP, env.URL+"/api/v1/events/stream?project_id="+pidAStr+
		"&after_id="+strconv.FormatInt(resetID, 10))
	defer func() { _ = sseResp2.Body.Close() }()
	framer2 := newSmokeFramer(sseResp2.Body)
	noMore := framer2.NextN(t, 1, 200*time.Millisecond)
	assert.Len(t, noMore, 0, "no further frames after reset cursor")
}

// pollNextAfterID issues a GET poll and returns next_after_id.
func pollNextAfterID(t *testing.T, client *http.Client, url string) int64 {
	t.Helper()
	resp, err := client.Get(url) //nolint:gosec // G107: test-only loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	var b struct {
		NextAfterID int64 `json:"next_after_id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&b))
	return b.NextAfterID
}

// openSSEAt opens an SSE GET with Accept: text/event-stream. The body is
// returned with the connection's full byte stream intact — smokeFramer's
// per-line scanner skips comment lines (": connected\n\n", heartbeats), so
// callers can wrap with newSmokeFramer without a fixed-byte preamble skip
// (which used to over- or under-consume bytes when frames raced the comment).
func openSSEAt(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req) //nolint:gosec // G107: test-only loopback
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	return resp
}

type smokeSSEFrame struct {
	id    string
	event string
	data  string
}

// smokeFramer streams SSE frames so callers can pull them one at a time
// across multiple test phases without re-creating the bufio.Reader each call
// (which would race with any in-flight ReadString or drop bytes the previous
// reader had buffered).
type smokeFramer struct {
	framesCh chan smokeSSEFrame
}

func newSmokeFramer(body io.Reader) *smokeFramer {
	f := &smokeFramer{framesCh: make(chan smokeSSEFrame, 16)}
	go func() {
		defer close(f.framesCh)
		rd := bufio.NewReader(body)
		cur := smokeSSEFrame{}
		for {
			s, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line := strings.TrimRight(s, "\r\n")
			switch {
			case line == "":
				if cur.id != "" || cur.event != "" || cur.data != "" {
					f.framesCh <- cur
					cur = smokeSSEFrame{}
				}
			case strings.HasPrefix(line, ":"):
				// heartbeat / comment — ignore
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return f
}

func (f *smokeFramer) Next(t *testing.T, timeout time.Duration) (smokeSSEFrame, bool) {
	t.Helper()
	select {
	case fr, ok := <-f.framesCh:
		return fr, ok
	case <-time.After(timeout):
		return smokeSSEFrame{}, false
	}
}

func (f *smokeFramer) NextN(t *testing.T, n int, timeout time.Duration) []smokeSSEFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var frames []smokeSSEFrame
	for len(frames) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return frames
		}
		fr, ok := f.Next(t, remaining)
		if !ok {
			return frames
		}
		frames = append(frames, fr)
	}
	return frames
}
