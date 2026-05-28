package daemon_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func mkProject(t *testing.T, env *testenv.Env, _ string, name string) int64 {
	t.Helper()
	p, err := env.DB.CreateProject(context.Background(), name)
	require.NoError(t, err)
	// Register env for issuePath's int64→short_id resolution.
	currentEnvForIssuePath = env
	t.Cleanup(func() {
		if currentEnvForIssuePath == env {
			currentEnvForIssuePath = nil
		}
	})
	return p.ID
}

func mkIssue(t *testing.T, env *testenv.Env, projectID int64, title string) db.Issue {
	t.Helper()
	is, _ := mkIssueWithEvent(t, env, projectID, title)
	return is
}

func mkIssueWithEvent(t *testing.T, env *testenv.Env, projectID int64, title string) (db.Issue, db.Event) {
	t.Helper()
	is, evt, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID, Title: title, Author: "tester",
	})
	require.NoError(t, err)
	return is, evt
}

// setupLiveSSE creates a project, injects a sentinel issue, opens an SSE
// stream just before that sentinel, and consumes the sentinel frame so the
// returned framer is parked in the live wakeup loop. Use this when a test
// needs to exercise post-drain handler behavior (e.g. broadcaster races,
// purge-reset rechecks) and must not have the events under test land in the
// initial drain phase.
func setupLiveSSE(t *testing.T, env *testenv.Env) (int64, db.Issue, *sseFramer) {
	t.Helper()
	pid := mkProject(t, env, "github.com/test/a", "a")
	sentinelIssue, sentinelEvt := mkIssueWithEvent(t, env, pid, "sentinel")

	hwm, err := env.DB.MaxEventID(context.Background())
	require.NoError(t, err)
	resp := openSSE(t, env, "after_id="+strconv.FormatInt(hwm-1, 10), nil)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, 200, resp.StatusCode)

	framer := newSSEFramer(resp.Body)
	first, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "sentinel drain frame should arrive")
	require.Equal(t, strconv.FormatInt(sentinelEvt.ID, 10), first.id)
	return pid, sentinelIssue, framer
}

func TestPollEvents_EmptyResultIsNonNullArray(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envGetRaw(t, env, "/api/v1/events?after_id=0&limit=10")
	require.Equal(t, 200, resp.StatusCode)
	body := string(bs)
	assert.Contains(t, body, `"events":[]`, "must be empty array, never null")
	assert.Contains(t, body, `"reset_required":false`)
	assert.Contains(t, body, `"next_after_id":0`)
}

func TestPollEvents_ReturnsEventsAndAdvancesCursor(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	first := mkIssue(t, env, pid, "first")
	mkIssue(t, env, pid, "second")
	project, err := env.DB.ProjectByID(context.Background(), pid)
	require.NoError(t, err)

	var b struct {
		ResetRequired bool `json:"reset_required"`
		Events        []struct {
			EventID      int64   `json:"event_id"`
			Type         string  `json:"type"`
			ProjectUID   string  `json:"project_uid"`
			IssueUID     *string `json:"issue_uid"`
			IssueShortID *string `json:"issue_short_id"`
		} `json:"events"`
		NextAfterID int64 `json:"next_after_id"`
	}
	envGetJSON(t, env, "/api/v1/events?after_id=0&limit=10", &b)
	require.Len(t, b.Events, 2)
	assert.Equal(t, int64(1), b.Events[0].EventID)
	assert.Equal(t, int64(2), b.Events[1].EventID)
	assert.Equal(t, "issue.created", b.Events[0].Type)
	assert.Equal(t, project.UID, b.Events[0].ProjectUID)
	require.NotNil(t, b.Events[0].IssueUID)
	assert.Equal(t, first.UID, *b.Events[0].IssueUID)
	require.NotNil(t, b.Events[0].IssueShortID, "envelope must surface joined issue_short_id")
	assert.Equal(t, first.ShortID, *b.Events[0].IssueShortID)
	assert.Equal(t, int64(2), b.NextAfterID, "advances to max event id")
	assert.False(t, b.ResetRequired)
}

func TestPollEvents_OmitsTokenEvents(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkIssue(t, env, pid, "first")
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	var b struct {
		Events []struct {
			EventID int64  `json:"event_id"`
			Type    string `json:"type"`
		} `json:"events"`
	}
	envGetJSON(t, env, "/api/v1/events?after_id=0&limit=10", &b)
	require.Len(t, b.Events, 1)
	assert.Equal(t, "issue.created", b.Events[0].Type)
}

// TestEvents_PayloadIncludesShortIDNotNumber pins the wire-level rename:
// each event row emits issue_short_id (joined from issues.short_id) and
// issue_uid, and does NOT carry an issue_number field. The check is on raw
// JSON keys (not a typed struct) so a regression that re-introduces
// issue_number would surface.
func TestEvents_PayloadIncludesShortIDNotNumber(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	created := mkIssue(t, env, pid, "evtest")

	var raw struct {
		Events []map[string]any `json:"events"`
	}
	envGetJSON(t, env, "/api/v1/projects/"+strconv.FormatInt(pid, 10)+"/events?after_id=0&limit=10", &raw)
	require.NotEmpty(t, raw.Events, "expected at least one event")
	first := raw.Events[0]
	assert.Equal(t, created.ShortID, first["issue_short_id"])
	assert.Equal(t, created.UID, first["issue_uid"])
	_, hasNum := first["issue_number"]
	assert.False(t, hasNum, "issue_number must not appear in event envelope")
}

func TestPollEvents_UIDsIncludeRelatedIssue(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	from := mkIssue(t, env, pid, "from")
	to := mkIssue(t, env, pid, "to")
	project, err := env.DB.ProjectByID(context.Background(), pid)
	require.NoError(t, err)
	_, _, err = env.DB.CreateLinkAndEvent(context.Background(), db.CreateLinkParams{
		ProjectID: pid, FromIssueID: from.ID, ToIssueID: to.ID, Type: "blocks", Author: "tester",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: from.ID,
		FromShortID: from.ShortID, FromUID: from.UID,
		ToShortID: to.ShortID, ToUID: to.UID, Actor: "tester",
	})
	require.NoError(t, err)

	var b struct {
		Events []struct {
			Type                string  `json:"type"`
			ProjectUID          string  `json:"project_uid"`
			IssueUID            *string `json:"issue_uid"`
			IssueShortID        *string `json:"issue_short_id"`
			RelatedIssueUID     *string `json:"related_issue_uid"`
			RelatedIssueShortID *string `json:"related_issue_short_id"`
		} `json:"events"`
	}
	envGetJSON(t, env, "/api/v1/events?after_id=2&limit=10", &b)
	require.Len(t, b.Events, 1)
	assert.Equal(t, "issue.linked", b.Events[0].Type)
	assert.Equal(t, project.UID, b.Events[0].ProjectUID)
	require.NotNil(t, b.Events[0].IssueUID)
	require.NotNil(t, b.Events[0].RelatedIssueUID)
	assert.Equal(t, from.UID, *b.Events[0].IssueUID)
	assert.Equal(t, to.UID, *b.Events[0].RelatedIssueUID)
	require.NotNil(t, b.Events[0].IssueShortID, "envelope must surface issue_short_id for linked events")
	require.NotNil(t, b.Events[0].RelatedIssueShortID, "envelope must surface related_issue_short_id for linked events")
	assert.Equal(t, from.ShortID, *b.Events[0].IssueShortID)
	assert.Equal(t, to.ShortID, *b.Events[0].RelatedIssueShortID)
}

func TestPollEvents_NextAfterIDEchoesAfterIDOnEmpty(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkIssue(t, env, pid, "only")

	resp, bs := envGetRaw(t, env, "/api/v1/events?after_id=99&limit=10")
	require.Equal(t, 200, resp.StatusCode)
	body := string(bs)
	assert.Contains(t, body, `"next_after_id":99`)
	assert.Contains(t, body, `"events":[]`)
}

func TestPollEvents_PerProjectFiltersOtherProjects(t *testing.T) {
	env := testenv.New(t)
	pa := mkProject(t, env, "github.com/test/a", "a")
	pb := mkProject(t, env, "github.com/test/b", "b")
	mkIssue(t, env, pa, "a1")
	mkIssue(t, env, pb, "b1")

	var b struct {
		Events []struct {
			ProjectID int64 `json:"project_id"`
		} `json:"events"`
	}
	envGetJSON(t, env, "/api/v1/projects/"+strconv.FormatInt(pa, 10)+"/events?after_id=0&limit=10", &b)
	require.Len(t, b.Events, 1)
	assert.Equal(t, pa, b.Events[0].ProjectID)
}

func TestPollEvents_ResetRequiredAfterPurge(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	is := mkIssue(t, env, pid, "doomed")
	_, err := env.DB.PurgeIssue(context.Background(), is.ID, "tester", nil)
	require.NoError(t, err)

	// Cursor below the reset → reset_required:true
	var b struct {
		ResetRequired bool  `json:"reset_required"`
		ResetAfterID  int64 `json:"reset_after_id"`
		Events        []struct {
			EventID int64 `json:"event_id"`
		} `json:"events"`
		NextAfterID int64 `json:"next_after_id"`
	}
	envGetJSON(t, env, "/api/v1/events?after_id=0&limit=10", &b)
	assert.True(t, b.ResetRequired)
	assert.Greater(t, b.ResetAfterID, int64(0))
	assert.Equal(t, b.ResetAfterID, b.NextAfterID, "next_after_id == reset_after_id when reset")
	assert.Len(t, b.Events, 0)
}

func TestPollEvents_LimitClampsAt1000(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	for i := 0; i < 3; i++ {
		mkIssue(t, env, pid, "x")
	}
	resp, _ := envGetRaw(t, env, "/api/v1/events?after_id=0&limit=99999")
	assert.Equal(t, 200, resp.StatusCode, "values >1000 must clamp silently, not 400")
}

func TestPollEvents_LimitNonPositiveIs400(t *testing.T) {
	env := testenv.New(t)
	for _, q := range []string{"after_id=0&limit=0", "after_id=0&limit=-5"} {
		resp, bs := envGetRaw(t, env, "/api/v1/events?"+q)
		assertAPIError(t, resp.StatusCode, bs, 400, "validation")
	}
}

func TestPollEvents_NegativeAfterIDIs400(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	pidStr := strconv.FormatInt(pid, 10)
	paths := []string{
		"/api/v1/events?after_id=-1",
		"/api/v1/projects/" + pidStr + "/events?after_id=-1",
	}
	for _, p := range paths {
		resp, bs := envGetRaw(t, env, p)
		assertAPIError(t, resp.StatusCode, bs, 400, "validation")
	}
}

func TestPollEvents_LimitNonNumericIs400(t *testing.T) {
	env := testenv.New(t)
	resp, _ := envGetRaw(t, env, "/api/v1/events?after_id=0&limit=foo")
	assert.Equal(t, 400, resp.StatusCode)
}

func TestPollEvents_LimitAbsentUsesDefault(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkIssue(t, env, pid, "first")
	mkIssue(t, env, pid, "second")

	// No limit query param at all — should default to 100 and return both rows.
	var b struct {
		Events []struct {
			EventID int64 `json:"event_id"`
		} `json:"events"`
	}
	envGetJSON(t, env, "/api/v1/events?after_id=0", &b)
	require.Len(t, b.Events, 2, "missing limit should default to pollLimitDefault, not reject the request")
}

// TestPollEvents_PerProject_NonPositiveProjectIDIs400 ensures that a request
// to /api/v1/projects/0/events does not silently fall through to the
// cross-project sentinel (projectID == 0) and leak every project's events.
func TestPollEvents_PerProject_NonPositiveProjectIDIs400(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envGetRaw(t, env, "/api/v1/projects/0/events?after_id=0&limit=10")
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

// TestPollEvents_PerProject_UnknownProjectIs404 mirrors sibling project-scoped
// handlers (e.g. issues/comments) which 404 with project_not_found rather
// than returning an empty list for a project that does not exist.
func TestPollEvents_PerProject_UnknownProjectIs404(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envGetRaw(t, env, "/api/v1/projects/9999/events?after_id=0&limit=10")
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_found")
}

func TestPollEvents_PerProject_SystemProjectIs404(t *testing.T) {
	env := testenv.New(t)
	sys, err := env.DB.SystemProject(context.Background())
	require.NoError(t, err)
	resp, bs := envGetRaw(t, env,
		"/api/v1/projects/"+strconv.FormatInt(sys.ID, 10)+"/events?after_id=0&limit=10")
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_found")
}

type sseFrame struct {
	id    string
	event string
	data  string
}

// sseFramer streams SSE frames from a single response body. Use Next() to
// pull one frame at a time so tests can synchronize between frames (e.g.,
// "wait for the drain frame before issuing the live mutation"). A single
// long-lived goroutine owns the bufio.Reader; concurrent ReadString on the
// same reader is undefined behavior.
type sseFramer struct {
	framesCh chan sseFrame
	doneCh   chan struct{}
}

func newSSEFramer(body io.Reader) *sseFramer {
	f := &sseFramer{
		framesCh: make(chan sseFrame, 8),
		doneCh:   make(chan struct{}),
	}
	go func() {
		defer close(f.framesCh)
		defer close(f.doneCh)
		rd := bufio.NewReader(body)
		cur := sseFrame{}
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
					cur = sseFrame{}
				}
			case strings.HasPrefix(line, ":"):
				// comment / heartbeat — ignore
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

func (f *sseFramer) Next(t *testing.T, timeout time.Duration) (sseFrame, bool) {
	t.Helper()
	select {
	case fr, ok := <-f.framesCh:
		return fr, ok
	case <-time.After(timeout):
		return sseFrame{}, false
	}
}

// readSSEFramesUntilN is kept for tests that don't need per-frame
// synchronization (e.g., simple drain assertions). Internally it builds an
// sseFramer and pulls n frames or until the deadline.
func readSSEFramesUntilN(t *testing.T, body interface {
	Read([]byte) (int, error)
	Close() error
}, n int, timeout time.Duration) []sseFrame {
	t.Helper()
	framer := newSSEFramer(body)
	deadline := time.Now().Add(timeout)
	var frames []sseFrame
	for len(frames) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return frames
		}
		fr, ok := framer.Next(t, remaining)
		if !ok {
			return frames
		}
		frames = append(frames, fr)
	}
	return frames
}

func openSSE(t *testing.T, env *testenv.Env, query string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		env.URL+"/api/v1/events/stream?"+query, nil)
	require.NoError(t, err)
	for k, vv := range header {
		req.Header[k] = vv
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := env.HTTP.Do(req) //nolint:gosec // G704: test server URL, not user-controlled
	require.NoError(t, err)
	return resp
}

func TestSSE_AcceptNegotiation(t *testing.T) {
	env := testenv.New(t)

	// Missing Accept → 406. envDoRaw doesn't add an Accept header on its own,
	// so this exercises the missing-header path.
	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/events/stream", nil, nil)
	assertAPIError(t, resp.StatusCode, bs, 406, "not_acceptable")

	// Wrong Accept → 406
	resp, _ = envDoRaw(t, env, http.MethodGet, "/api/v1/events/stream", nil,
		map[string]string{"Accept": "application/json"})
	assert.Equal(t, 406, resp.StatusCode)

	// Right Accept → 200
	sseResp := openSSE(t, env, "", http.Header{"Accept": []string{"text/event-stream"}})
	defer func() { _ = sseResp.Body.Close() }()
	assert.Equal(t, 200, sseResp.StatusCode)
	assert.Equal(t, "text/event-stream", sseResp.Header.Get("Content-Type"))

	// */* → 200
	sseResp2 := openSSE(t, env, "", http.Header{"Accept": []string{"*/*"}})
	defer func() { _ = sseResp2.Body.Close() }()
	assert.Equal(t, 200, sseResp2.StatusCode)
}

func TestSSE_CursorConflict(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/events/stream?after_id=5", nil,
		map[string]string{"Accept": "text/event-stream", "Last-Event-ID": "10"})
	assertAPIError(t, resp.StatusCode, bs, 400, "cursor_conflict")
}

// TestSSE_CursorConflictPresenceBased pins the rule that detection is on
// query/header *key presence*, not on a non-empty value. A request with both
// Last-Event-ID and a present-but-empty ?after_id= must surface
// cursor_conflict, not silently win for the header.
func TestSSE_CursorConflictPresenceBased(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/events/stream?after_id=", nil,
		map[string]string{"Accept": "text/event-stream", "Last-Event-ID": "5"})
	assertAPIError(t, resp.StatusCode, bs, 400, "cursor_conflict")
}

// TestSSE_NonGETReturnsAllowHeader pins that 405 responses include `Allow: GET`
// so a misrouted client can recover without scraping the message.
func TestSSE_NonGETReturnsAllowHeader(t *testing.T) {
	env := testenv.New(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		resp, _ := envDoRaw(t, env, method, "/api/v1/events/stream", nil, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "method=%s", method)
		assert.Equal(t, http.MethodGet, resp.Header.Get("Allow"), "method=%s", method)
	}
}

func TestSSE_HandshakeWritesConnectedComment(t *testing.T) {
	env := testenv.New(t)
	resp := openSSE(t, env, "", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "keep-alive", resp.Header.Get("Connection"))
	// Read first 16 bytes; should contain ": connected\n\n".
	buf := make([]byte, 16)
	_, err := resp.Body.Read(buf)
	require.NoError(t, err)
	assert.Contains(t, string(buf), ": connected")
}

func TestSSE_DrainEmitsExistingEventsInOrder(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkIssue(t, env, pid, "first")
	mkIssue(t, env, pid, "second")
	mkIssue(t, env, pid, "third")

	resp := openSSE(t, env, "after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)

	frames := readSSEFramesUntilN(t, resp.Body, 3, 2*time.Second)
	require.Len(t, frames, 3)
	assert.Equal(t, "1", frames[0].id)
	assert.Equal(t, "issue.created", frames[0].event)
	assert.Equal(t, "2", frames[1].id)
	assert.Equal(t, "3", frames[2].id)
}

func TestSSE_DrainOmitsTokenEvents(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkIssue(t, env, pid, "first")
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp := openSSE(t, env, "after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	framer := newSSEFramer(resp.Body)

	first, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "issue frame should arrive")
	assert.Equal(t, "issue.created", first.event)
	_, ok = framer.Next(t, 150*time.Millisecond)
	assert.False(t, ok, "token.created must not be emitted as a drain frame")
}

func TestSSE_PerProjectFilterExcludesOtherProjects(t *testing.T) {
	env := testenv.New(t)
	pa := mkProject(t, env, "github.com/test/a", "a")
	pb := mkProject(t, env, "github.com/test/b", "b")
	mkIssue(t, env, pa, "a1")
	mkIssue(t, env, pb, "b1")

	resp := openSSE(t, env, "project_id="+strconv.FormatInt(pa, 10)+"&after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()
	frames := readSSEFramesUntilN(t, resp.Body, 1, 2*time.Second)
	require.Len(t, frames, 1)
	assert.Equal(t, "1", frames[0].id, "should see only project A's event 1, not project B's event 2")
}

func TestSSE_ResetWhenCursorInsidePurgeGap(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	is := mkIssue(t, env, pid, "doomed")
	_, err := env.DB.PurgeIssue(context.Background(), is.ID, "tester", nil)
	require.NoError(t, err)

	resp := openSSE(t, env, "after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()

	frames := readSSEFramesUntilN(t, resp.Body, 1, 2*time.Second)
	require.Len(t, frames, 1)
	assert.Equal(t, "sync.reset_required", frames[0].event)
	assert.NotEmpty(t, frames[0].id)
	assert.Contains(t, frames[0].data, `"reset_after_id":`+frames[0].id)
}

func TestSSE_DrainFollowedByLiveBroadcast(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkIssue(t, env, pid, "first")

	resp := openSSE(t, env, "after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()
	framer := newSSEFramer(resp.Body)

	// Wait for the drain frame BEFORE creating the second issue. Otherwise
	// the second issue's commit could land before the SSE handler queries
	// EventsAfter, putting both events in the drain phase and not exercising
	// the live broadcast path.
	first, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "drain frame should arrive")
	assert.Equal(t, "1", first.id)
	assert.Equal(t, "issue.created", first.event)

	// Now create a second issue via HTTP so the handler fires a live broadcast.
	envPostJSON(t, env, projectPath(pid)+"/issues",
		map[string]string{"title": "second", "actor": "tester"}, nil)

	second, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "live frame should arrive after the broadcast")
	assert.Equal(t, "2", second.id)
	assert.Equal(t, "issue.created", second.event)
}

func TestSSE_LivePhaseOmitsTokenEvents(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")

	resp := openSSE(t, env, "after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()
	framer := newSSEFramer(resp.Body)

	sys, err := env.DB.SystemProject(context.Background())
	require.NoError(t, err)
	_, tokenEvt, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	env.Broadcaster.Broadcast(daemon.StreamMsg{Kind: "event", Event: &tokenEvt, ProjectID: sys.ID})
	_, ok := framer.Next(t, 150*time.Millisecond)
	assert.False(t, ok, "token.created must not be emitted as a live frame")

	envPostJSON(t, env, projectPath(pid)+"/issues",
		map[string]string{"title": "visible", "actor": "tester"}, nil)
	visible, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "normal live event should still arrive")
	assert.Equal(t, "issue.created", visible.event)
}

func TestSSE_LiveResetClosesStream(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	is := mkIssue(t, env, pid, "doomed")

	resp := openSSE(t, env, "after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()
	framer := newSSEFramer(resp.Body)

	// Wait for the drain frame to be observed BEFORE issuing the purge.
	// Otherwise the purge cascade might delete the issue.created event row
	// before the drain query runs, so drain returns empty and the test only
	// sees the reset frame instead of the documented {drain, reset} pair.
	first, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "drain frame should arrive")
	assert.Equal(t, "issue.created", first.event)

	project, perr := env.DB.ProjectByID(context.Background(), pid)
	require.NoError(t, perr)
	purgeResp, _ := envDoRaw(t, env, http.MethodPost,
		issuePathRef(pid, is.ShortID, "actions/purge"),
		map[string]string{"actor": "tester"},
		map[string]string{"X-Kata-Confirm": "PURGE " + project.Name + "#" + is.ShortID})
	require.Equal(t, 200, purgeResp.StatusCode)

	reset, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "reset frame should arrive after purge")
	assert.Equal(t, "sync.reset_required", reset.event)

	// Reset frames are terminal: the handler returns and the body must EOF.
	// Without this assertion, a regression that keeps the SSE connection open
	// after sync.reset_required would still pass.
	select {
	case <-framer.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after reset frame")
	}
}

func TestSSE_ParentReplaceEmitsTwoFrames(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	first := mkIssue(t, env, pid, "first")   // initial parent
	second := mkIssue(t, env, pid, "second") // replacement parent
	child := mkIssue(t, env, pid, "child")

	// Initial parent link child → first.
	envPostJSON(t, env, issuePathRef(pid, child.ShortID, "links"),
		map[string]any{"actor": "tester", "type": "parent", "to_ref": first.ShortID}, nil)

	// Subscribe AFTER the initial link so we don't see its frame in the drain.
	maxID, err := env.DB.MaxEventID(context.Background())
	require.NoError(t, err)
	sseResp := openSSE(t, env, "after_id="+strconv.FormatInt(maxID, 10), nil)
	defer func() { _ = sseResp.Body.Close() }()

	// Re-parent child → second with replace.
	envPostJSON(t, env, issuePathRef(pid, child.ShortID, "links"),
		map[string]any{"actor": "tester", "type": "parent", "to_ref": second.ShortID, "replace": true}, nil)

	// Live phase delivers two frames in order: issue.unlinked then issue.linked.
	frames := readSSEFramesUntilN(t, sseResp.Body, 2, 2*time.Second)
	require.Len(t, frames, 2)
	assert.Equal(t, "issue.unlinked", frames[0].event)
	assert.Equal(t, "issue.linked", frames[1].event)
}

func TestSSE_UnknownProjectIDReturns404(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/events/stream?project_id=99999", nil,
		map[string]string{"Accept": "text/event-stream"})
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_found")
}

func TestSSE_SystemProjectIDReturns404(t *testing.T) {
	env := testenv.New(t)
	sys, err := env.DB.SystemProject(context.Background())
	require.NoError(t, err)
	resp, bs := envDoRaw(t, env, http.MethodGet,
		"/api/v1/events/stream?project_id="+strconv.FormatInt(sys.ID, 10), nil,
		map[string]string{"Accept": "text/event-stream"})
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_found")
}

func TestSSE_LiveHeartbeatKeepsConnectionAlive(t *testing.T) {
	// Connection should stay open for >100ms with empty DB and no events.
	// We don't wait for a 25s heartbeat; we only verify the stream isn't
	// immediately torn down.
	env := testenv.New(t)
	resp := openSSE(t, env, "after_id=0", nil)
	defer func() { _ = resp.Body.Close() }()

	// Read the : connected\n\n preamble.
	buf := make([]byte, 16)
	_, err := resp.Body.Read(buf)
	require.NoError(t, err)

	// No frames should arrive in 100ms with an empty DB.
	frames := readSSEFramesUntilN(t, resp.Body, 1, 100*time.Millisecond)
	assert.Len(t, frames, 0)
}
