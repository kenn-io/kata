package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"hegel.dev/go/hegel"

	native "go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

type peekComment struct {
	UID       string    `json:"uid"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// The latest readable comment is ordered by creation time, then comment UID,
// whatever order the daemon returns comments in.
func TestCronIssuePeekLatestCommentProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		comments := []peekComment{}
		for i := range 3 {
			uid := hegel.Draw(ht, hegel.Text())
			seconds := hegel.Draw(ht, hegel.Integers[int64](-62135596800, 253402300799))
			comments = append(comments, peekComment{fmt.Sprintf("%s:%d", uid, i), "example-author", fmt.Sprintf("body-%d", i), time.Unix(seconds, 0).UTC()})
		}
		// Force a timestamp tie so the UID tiebreak is always exercised.
		comments[1].CreatedAt = comments[0].CreatedAt
		want := comments[0]
		for _, c := range comments[1:] {
			if c.CreatedAt.After(want.CreatedAt) || c.CreatedAt.Equal(want.CreatedAt) && c.UID > want.UID {
				want = c
			}
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/v1/projects/7/issues/issue-a" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"issue": map[string]string{"uid": "issue-a", "short_id": "ab12", "title": "Example issue"}, "comments": comments})
		}))
		defer server.Close()
		got, err := NewClient(server.URL, server.Client()).CronIssuePeek(context.Background(), 7, "issue-a")
		if err != nil {
			ht.Fatal(err)
		}
		if got.IssueUID != "issue-a" || got.ShortID != "ab12" || got.Title != "Example issue" || got.Latest == nil || got.Latest.UID != want.UID || got.Latest.Body != want.Body || !got.Latest.CreatedAt.Equal(want.CreatedAt) {
			ht.Fatalf("latest comment = %+v, want %+v", got.Latest, want)
		}
	}, hegel.WithTestCases(200))
}

func newCorePeekHost(t *testing.T, handler http.HandlerFunc) *cronHost {
	t.Helper()
	server := httptest.NewServer(handler)
	n := initialModel(Options{})
	n.api = NewClient(server.URL, server.Client())
	n.scope = scope{projectID: 7, projectName: "example-project"}
	h := newCronHost(n)
	h.tab, h.width, h.height, h.dirty = 2, 120, 32, false
	runs := []records.Run{}
	for i, issue := range []string{"issue-a", "issue-b"} {
		runs = append(runs, records.Run{UID: fmt.Sprintf("run-%d", i), ProjectID: 7, ProjectKey: "7", IssueUID: new(issue), Status: "succeeded", Summary: native.Summary{Version: 1, Message: "own summary marker"}, CreatedAt: time.Date(2026, 1, 1, 8-i, 0, 0, 0, time.UTC)})
	}
	h.catalogs = map[int64]cronCatalog{7: {name: "example-project", uid: "project-uid", runs: runs, runsLoaded: true}}
	h.updateCronBoard()
	t.Cleanup(func() {
		_ = h.Close()
		server.CloseClientConnections()
		server.Close()
	})
	return h
}

func corePeekHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/v1/projects/7/issues/issue-a" {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write([]byte(`{"issue":{"uid":"issue-a","short_id":"ab12","title":"Example issue"},"comments":[` +
		`{"uid":"comment-b","author":"example-author","body":"older comment","created_at":"2026-01-01T07:00:00Z"},` +
		`{"uid":"comment-a","author":"example-author","body":"protected comment marker","created_at":"2026-01-01T08:00:00Z"}]}`))
}

func coreExecute(cmd tea.Cmd, out chan<- tea.Msg) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			coreExecute(child, out)
		}
	} else if msg != nil {
		out <- msg
	}
}

// corePeekReply lets the host start the selected run's issue read and returns
// the reply without delivering it.
func corePeekReply(t *testing.T, h *cronHost) cronPeekMsg {
	t.Helper()
	_, cmd := h.Update(struct{}{})
	out := make(chan tea.Msg, 16)
	go coreExecute(cmd, out)
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg := <-out:
			if reply, ok := msg.(cronPeekMsg); ok {
				return reply
			}
		case <-timeout:
			t.Fatal("selected run did not produce its linked issue peek within 2s")
		}
	}
}

func TestCronPeekShowsSummaryAndLatestComment(t *testing.T) {
	h := newCorePeekHost(t, corePeekHandler)
	reply := corePeekReply(t, h)
	if text := ansi.Strip(h.View().Content); !strings.Contains(text, "Loading issue") || !strings.Contains(text, "own summary marker") {
		t.Fatalf("pending peek hides its loading state or own summary:\n%s", text)
	}
	h.Update(reply)
	text := ansi.Strip(h.View().Content)
	for _, want := range []string{"own summary marker", "ab12 Example issue", "protected comment marker"} {
		if !strings.Contains(text, want) {
			t.Fatalf("peek lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "older comment") {
		t.Fatalf("peek showed an older comment:\n%s", text)
	}
}

// A reply captured under an old selection, project, or authority must never
// restore that context, however often it is replayed.
func TestCronPeekFencesStaleReplies(t *testing.T) {
	transitions := map[string]func(h *cronHost){
		"selection":  func(h *cronHost) { h.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) },
		"connection": func(h *cronHost) { h.native.connGen++; h.Update(struct{}{}) },
		"project":    func(h *cronHost) { h.native.scope.projectID = 8; h.Update(struct{}{}) },
		"project UID": func(h *cronHost) {
			cat := h.catalogs[7]
			cat.uid = "replacement-uid"
			h.catalogs[7] = cat
			h.Update(struct{}{})
		},
		"authority": func(h *cronHost) { h.native.activeAuthRejected = true; h.Update(struct{}{}) },
		"removed": func(h *cronHost) {
			cat := h.catalogs[7]
			cat.runs = nil
			h.catalogs[7] = cat
			h.Update(struct{}{})
		},
		"close": func(h *cronHost) {
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, transition := range transitions {
		for _, delivered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delivered=%t", name, delivered), func(t *testing.T) {
				h := newCorePeekHost(t, corePeekHandler)
				reply := corePeekReply(t, h)
				if delivered {
					h.Update(reply)
					if !strings.Contains(ansi.Strip(h.View().Content), "protected comment marker") {
						t.Fatal("readable linked issue context was not rendered")
					}
				}
				transition(h)
				for range 3 {
					h.Update(reply)
				}
				if text := ansi.Strip(h.View().Content); strings.Contains(text, "protected comment marker") {
					t.Fatalf("stale issue context restored:\n%s", text)
				}
			})
		}
	}
}

// corePeekReplyFrom runs cmd's children concurrently, so a blocking native
// command cannot hide the peek read, and returns the first peek reply.
func corePeekReplyFrom(t *testing.T, cmd tea.Cmd) cronPeekMsg {
	t.Helper()
	out := make(chan tea.Msg, 64)
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, child := range batch {
				go run(child)
			}
		} else if msg != nil {
			out <- msg
		}
	}
	go run(cmd)
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg := <-out:
			if reply, ok := msg.(cronPeekMsg); ok {
				return reply
			}
		case <-timeout:
			t.Fatal("issue event did not refresh the selected run's linked issue peek within 2s")
		}
	}
}

// A live change to the peeked issue, or a reset or reconnect that may have
// missed one, refreshes the selected run's context in place, and a reply read
// before a later change never replaces newer context.
func TestCronPeekRefreshesOnIssueEvents(t *testing.T) {
	signals := map[string]tea.Msg{"reset": resetRequiredMsg{}, "reconnect": sseStatusMsg{state: sseConnected}}
	for _, eventType := range []string{"issue.commented", "issue.comment_edited", "issue.updated"} {
		signals[eventType] = eventReceivedMsg{eventType: eventType, projectID: 7, projectUID: "project-uid", issueUID: "issue-a", commentUID: "comment-a"}
	}
	for name, signal := range signals {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			title, body := "Example issue", "first comment marker"
			h := newCorePeekHost(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/v1/projects/7/issues/issue-a" {
					http.NotFound(w, r)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issue":    map[string]string{"uid": "issue-a", "short_id": "ab12", "title": title},
					"comments": []peekComment{{"comment-a", "example-author", body, time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}},
				})
			})
			change := func(nextTitle, nextBody string) tea.Cmd {
				mu.Lock()
				title, body = nextTitle, nextBody
				mu.Unlock()
				_, cmd := h.Update(signal)
				return cmd
			}
			selected := func() string {
				run, ok := h.board.SelectedRun()
				if !ok {
					t.Fatal("no run selected")
				}
				return run.UID
			}
			first := corePeekReply(t, h)
			h.Update(first)
			run := selected()

			// An event for another issue leaves the peek and its read alone.
			if event, ok := signal.(eventReceivedMsg); ok {
				generation := h.peekGeneration
				event.issueUID = "issue-b"
				h.Update(event)
				if h.peekGeneration != generation {
					t.Fatal("an unrelated issue event replaced the peek read")
				}
			}

			older := corePeekReplyFrom(t, change("Renamed issue", "second comment marker"))
			newer := corePeekReplyFrom(t, change("Retitled issue", "third comment marker"))
			// A reset or reconnect replaces cached cron evidence, so only an
			// issue change keeps the old context visible meanwhile.
			if _, ok := signal.(eventReceivedMsg); ok {
				if text := ansi.Strip(h.View().Content); !strings.Contains(text, "first comment marker") || strings.Contains(text, "Loading issue") {
					t.Fatalf("refresh hid the visible peek:\n%s", text)
				}
			}
			h.Update(newer)
			for _, late := range []cronPeekMsg{older, first, older} {
				h.Update(late)
			}
			if selected() != run {
				t.Fatalf("selection moved from %s to %s", run, selected())
			}
			text := ansi.Strip(h.View().Content)
			if !strings.Contains(text, "ab12 Retitled issue") || !strings.Contains(text, "third comment marker") {
				t.Fatalf("peek lacks the latest issue context:\n%s", text)
			}
			for _, stale := range []string{"first comment marker", "second comment marker", "Renamed issue"} {
				if strings.Contains(text, stale) {
					t.Fatalf("late reply restored %q:\n%s", stale, text)
				}
			}
		})
	}
}

func bearerClient(env *testenv.Env, token string) *Client {
	base := env.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return NewClient(env.URL, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		return base.RoundTrip(r)
	})})
}

// Deleting, restoring, snapshotting, or moving the peeked issue rereads it.
// The peek never keeps context the reader can no longer read, shows a
// restored issue again, and follows a move into another readable project by
// UID; i then opens the issue there. Replies read before a transition stay
// discarded.
func TestCronPeekFollowsIssueLifecycle(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-test-token"), testenv.WithRequireTokenIdentity())
	ctx := context.Background()
	admin := bearerClient(env, "admin-test-token")
	var projects []db.Project
	for _, name := range []string{"spoke-project", "hub-project", "restricted-project"} {
		p, err := env.DB.CreateProject(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		projects = append(projects, p)
	}
	p, hub, restricted := projects[0], projects[1], projects[2]
	team, _, err := env.DB.CreateTeam(ctx, "engineering", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.SetTeamMembership(ctx, team.UID, "admin", true, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.DB.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: restricted.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"admin", "reader"} {
		if _, _, err := env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: actor + "-test-token", Actor: actor, AdminActor: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Example issue", Author: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.AddComment(ctx, p.ID, issue.UID, "first comment marker", "admin"); err != nil {
		t.Fatal(err)
	}
	action := func(project int64, name, body string, header map[string]string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/%s", env.URL, project, issue.UID, name), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin-test-token")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := env.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(name, resp.Status, string(raw))
		}
	}
	move := func(from, to db.Project) func() {
		return func() {
			current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			if err != nil {
				t.Fatal(err)
			}
			action(from.ID, "move", fmt.Sprintf(`{"actor":"admin","to_project_uid":%q}`, to.UID), map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, current.Revision)})
		}
	}

	n := initialModel(Options{})
	n.api = bearerClient(env, "reader-test-token")
	n.scope = scope{projectID: p.ID, projectName: p.Name}
	h := newCronHost(n)
	h.tab, h.width, h.height, h.dirty = 2, 120, 32, false
	key := fmt.Sprint(p.ID)
	h.catalogs = map[int64]cronCatalog{p.ID: {name: p.Name, uid: p.UID, runsLoaded: true, runs: []records.Run{{UID: "run-0", ProjectID: p.ID, ProjectKey: key, IssueUID: &issue.UID, Status: "succeeded", Summary: native.Summary{Version: 1, Message: "own summary marker"}, CreatedAt: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}}}}
	h.updateCronBoard()
	t.Cleanup(func() { _ = h.Close() })

	replies := []cronPeekMsg{corePeekReply(t, h)}
	h.Update(replies[0])
	steps := []struct {
		event, want, gone string
		change            func()
	}{
		{"issue.soft_deleted", "Issue not readable", "first comment marker", func() {
			action(p.ID, "delete", `{"actor":"admin"}`, map[string]string{"X-Kata-Confirm": fmt.Sprintf("DELETE %s#%s", p.Name, issue.ShortID)})
		}},
		{"issue.restored", "first comment marker", "Issue not readable", func() { action(p.ID, "restore", `{"actor":"admin"}`, nil) }},
		{"issue.snapshot", "snapshot comment marker", "first comment marker", func() {
			if _, err := admin.AddComment(ctx, p.ID, issue.UID, "snapshot comment marker", "admin"); err != nil {
				t.Fatal(err)
			}
		}},
		{"issue.moved", "Issue not readable", "snapshot comment marker", move(p, restricted)},
		{"issue.moved", "snapshot comment marker", "Issue not readable", move(restricted, hub)},
	}
	for _, step := range steps {
		step.change()
		_, cmd := h.Update(eventReceivedMsg{eventType: step.event, projectID: p.ID, projectUID: p.UID, issueUID: issue.UID})
		reply := corePeekReplyFrom(t, cmd)
		h.Update(reply)
		for _, late := range replies {
			h.Update(late)
		}
		replies = append(replies, reply)
		if text := ansi.Strip(h.View().Content); !strings.Contains(text, step.want) || strings.Contains(text, step.gone) || !strings.Contains(text, "own summary marker") {
			t.Fatalf("after %s the peek lacks %q or keeps %q:\n%s", step.event, step.want, step.gone, text)
		}
	}

	_, cmd := h.Update(cronui.IssueMsg{ProjectKey: key, UID: issue.UID})
	drainCronSmoke(t, h, cmd)
	if h.native.view != viewDetail || h.native.detail.issue == nil || h.native.detail.issue.UID != issue.UID || h.native.detail.scopePID != hub.ID {
		t.Fatalf("i did not open the moved issue in its current project: view=%v scope=%d", h.native.view, h.native.detail.scopePID)
	}
}

func TestCronCloseCancelsPendingPeek(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	h := newCorePeekHost(t, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	_, cmd := h.Update(struct{}{})
	go coreExecute(cmd, make(chan tea.Msg, 16))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("peek did not start")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown retained the pending issue request")
	}
}

func TestCronPeekMissingOrUnreadableIssue(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newCorePeekHost(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == 200 {
					_, _ = w.Write([]byte(`{"issue":{"uid":"issue-a","short_id":"ab12","title":"Example issue"},"comments":[]}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"issue unavailable"}`))
				}
			})
			h.Update(corePeekReply(t, h))
			if status == 401 {
				if !h.native.activeAuthRejected || len(h.catalogs) != 0 {
					t.Fatal("issue auth rejection retained cron authority")
				}
				return
			}
			want := "Issue not readable"
			if status == 200 {
				want = "No comments"
			}
			if text := ansi.Strip(h.View().Content); !strings.Contains(text, want) || !strings.Contains(text, "own summary marker") {
				t.Fatalf("missing issue state %q or own summary:\n%s", want, text)
			}
		})
	}
}

// A moved issue's UID read keeps the peek's authority handling: a rejected
// credential clears cron access and a denial stays unreadable.
func TestCronPeekUIDFallbackAuthority(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newCorePeekHost(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/issues/issue-a" {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"code":"denied","message":"issue unavailable"}}`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"issue_not_found","message":"issue not found"}}`))
			})
			h.Update(corePeekReply(t, h))
			if status == 401 {
				if !h.native.activeAuthRejected || len(h.catalogs) != 0 {
					t.Fatal("UID read auth rejection retained cron authority")
				}
				return
			}
			if text := ansi.Strip(h.View().Content); !strings.Contains(text, "Issue not readable") {
				t.Fatalf("denied UID read is not unreadable:\n%s", text)
			}
		})
	}
}

func TestCronPeekWithoutLinkedIssue(t *testing.T) {
	h := newCorePeekHost(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unlinked run read an issue: %s", r.URL)
		http.NotFound(w, r)
	})
	cat := h.catalogs[7]
	cat.runs[0].IssueUID = nil
	h.catalogs[7] = cat
	h.updateCronBoard()
	if _, cmd := h.Update(struct{}{}); cmd != nil {
		go coreExecute(cmd, make(chan tea.Msg, 16))
	}
	if text := ansi.Strip(h.View().Content); !strings.Contains(text, "No issue linked") || !strings.Contains(text, "own summary marker") {
		t.Fatalf("unlinked run lacks its explicit state:\n%s", text)
	}
}

// An older daemon omits last_run; the job stays usable and is not mistaken
// for one that never ran. TestCronDaemonLastRunBeyondLoadedHistory covers the
// current daemon's explicit null and populated last_run.
func TestCronClientToleratesOlderDaemonJobs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"uid":"legacy","name":"legacy-job","definition":{}}]}`))
	}))
	defer server.Close()
	jobs, err := NewClient(server.URL, server.Client()).ListCronJobs(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "legacy-job" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].LastRunKnown || jobs[0].LastRun != nil {
		t.Fatalf("omitted last_run treated as authoritative: %+v", jobs[0])
	}
}

// The core TUI reads only the ordinary cron and issue endpoints.
func TestCronClientReadsOnlyCoreEndpoints(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.RawQuery != "" && r.URL.Path != "/api/v1/projects/7/cron/runs" {
			t.Errorf("unexpected core read: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/7/cron/jobs":
			_, _ = w.Write([]byte(`{"jobs":[]}`))
		case "/api/v1/projects/7/cron/workflows":
			_, _ = w.Write([]byte(`{"workflows":[]}`))
		case "/api/v1/projects/7/cron/runs":
			_, _ = w.Write([]byte(`{"runs":[]}`))
		case "/api/v1/projects/7/cron/runs/run-a":
			_, _ = w.Write([]byte(`{"run":{"uid":"run-a","summary":{"version":1,"message":"own summary marker"}}}`))
		case "/api/v1/projects/7/issues/issue-a":
			corePeekHandler(w, r)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := NewClient(server.URL, server.Client())
	ctx := context.Background()
	if _, err := c.ListCronJobs(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListCronWorkflows(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListCronRuns(ctx, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ShowCronRun(ctx, 7, "run-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CronIssuePeek(ctx, 7, "issue-a"); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatalf("core reads = %d, want 5", calls)
	}
}
