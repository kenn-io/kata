package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"hegel.dev/go/hegel"

	cronui "go.kenn.io/kata/internal/tui/cron"
)

const relatedURL = "https://agentsview.example/Base%2Fpath/sessions/Opaque:Value"

func closedEvent(id int64, projectUID, issueUID string, payload map[string]any) EventLogEntry {
	return EventLogEntry{ID: id, Type: "issue.closed", ProjectUID: projectUID, IssueUID: issueUID, Payload: payload}
}

func transcriptPayload(locator any) map[string]any {
	return map[string]any{"transcript": map[string]any{"url": locator}}
}

func TestAgentsViewCronRelatedURL(t *testing.T) {
	const project, issue = "project-uid", "issue-a"
	rows := []struct {
		name    string
		enabled bool
		events  []EventLogEntry
		want    string
	}{
		{"disabled", false, []EventLogEntry{closedEvent(1, project, issue, transcriptPayload(relatedURL))}, ""},
		{"native transcript", true, []EventLogEntry{closedEvent(1, project, issue, map[string]any{"transcript": map[string]any{"agent": "codex", "session_id": "0198d2c4-7f3e-7b1a-9c55-2b4e1d0a6f10", "url": relatedURL}})}, relatedURL},
		{"non-native agent", true, []EventLogEntry{closedEvent(1, project, issue, map[string]any{"transcript": map[string]any{"agent": "other-agent", "url": "https://agentsview.example/archive/sessions/opaque/value"}})}, "https://agentsview.example/archive/sessions/opaque/value"},
		{"url only", true, []EventLogEntry{closedEvent(1, project, issue, transcriptPayload(relatedURL))}, relatedURL},
		{"greatest id wins over order", true, []EventLogEntry{closedEvent(9, project, issue, transcriptPayload("https://agentsview.example/new")), closedEvent(3, project, issue, transcriptPayload("https://agentsview.example/old"))}, "https://agentsview.example/new"},
		{"newer unsafe is skipped", true, []EventLogEntry{closedEvent(3, project, issue, transcriptPayload("https://agentsview.example/old")), closedEvent(9, project, issue, transcriptPayload("javascript:alert(1)"))}, "https://agentsview.example/old"},
		{"other project", true, []EventLogEntry{closedEvent(1, "other-project", issue, transcriptPayload(relatedURL))}, ""},
		{"decimal project key", true, []EventLogEntry{closedEvent(1, "7", issue, transcriptPayload(relatedURL))}, ""},
		{"other subject with related uid", true, []EventLogEntry{{ID: 1, Type: "issue.closed", ProjectUID: project, IssueUID: "issue-b", RelatedIssueUID: issue, Payload: transcriptPayload(relatedURL)}}, ""},
		{"not a close", true, []EventLogEntry{{ID: 1, Type: "issue.commented", ProjectUID: project, IssueUID: issue, Payload: transcriptPayload(relatedURL)}}, ""},
		{"no transcript", true, []EventLogEntry{closedEvent(1, project, issue, map[string]any{"reason": "done"})}, ""},
		{"transcript not object", true, []EventLogEntry{closedEvent(1, project, issue, map[string]any{"transcript": relatedURL})}, ""},
		{"url not string", true, []EventLogEntry{closedEvent(1, project, issue, transcriptPayload(42))}, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := relatedCronTranscriptURL(row.enabled, []string{project}, issue, row.events); got != row.want {
				t.Fatalf("locator = %q, want %q", got, row.want)
			}
		})
	}
	if got := relatedCronTranscriptURL(true, []string{""}, issue, rows[3].events); got != "" {
		t.Fatalf("unresolved project authority returned %q", got)
	}
}

func TestAgentsViewCronSafeURL(t *testing.T) {
	for _, raw := range []string{relatedURL, "http://agentsview.example", "HTTPS://AgentsView.Example/Sessions/A%20B", "https://agentsview.example:8443/s/x"} {
		if !safeTranscriptURL(raw) {
			t.Errorf("rejected safe URL %q", raw)
		}
	}
	for _, raw := range []string{
		"", "/sessions/x", "//agentsview.example/x", "agentsview.example/x", "ftp://agentsview.example/x",
		"javascript:alert(1)", "file:///etc/passwd", "https:opaque", "https://", "https://:443/x",
		"https://user@agentsview.example/x", "https://user:secret@agentsview.example/x",
		"https://agentsview.example/x?", "https://agentsview.example/x?token=1", "https://agentsview.example/x#", "https://agentsview.example/x#frag",
		"https://agentsview.example/x\x00", "https://agentsview.example/x\x7f", "https://agentsview.example/\u0085x",
		" https://agentsview.example/x", "https://agentsview.example/x ", "https://agentsview.example/a b", "https://agentsview.example/\tx",
		"https://agentsview.example\\@evil.example/x",
	} {
		if safeTranscriptURL(raw) {
			t.Errorf("accepted unsafe URL %q", raw)
		}
	}
}

// Whatever the recorded value, an accepted locator is returned unchanged and
// never carries credentials, a query, a fragment, whitespace or controls; and
// events outside the exact project and subject never contribute.
func TestAgentsViewCronLocatorProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"https://", "http://", "HTTP://", "ftp://", "//", ""}))
		host := hegel.Draw(ht, hegel.SampledFrom([]string{"agentsview.example", "user@agentsview.example", "", "agentsview.example:8080"}))
		raw := prefix + host + hegel.Draw(ht, hegel.Text())
		project := hegel.Draw(ht, hegel.SampledFrom([]string{"project-uid", "7", "other-project"}))
		subject := hegel.Draw(ht, hegel.SampledFrom([]string{"issue-a", "issue-b"}))
		got := relatedCronTranscriptURL(true, []string{"project-uid"}, "issue-a", []EventLogEntry{closedEvent(hegel.Draw(ht, hegel.Integers[int64](1, 1<<40)), project, subject, transcriptPayload(raw))})
		if got == "" {
			return
		}
		if got != raw || project != "project-uid" || subject != "issue-a" {
			ht.Fatalf("accepted %q from project %q subject %q", got, project, subject)
		}
		if strings.ContainsAny(got, "?# \t\r\n@") || !strings.HasPrefix(strings.ToLower(got), "http") {
			ht.Fatalf("accepted unsafe locator %q", got)
		}
	}, hegel.WithTestCases(500))
}

type relatedDaemon struct {
	mu           sync.Mutex
	capabilities string
	pages        map[string]string // after_id → body
	ack          string
	eventsStatus int
	block        chan struct{}
	requests     []string
	// moved serves issue-a from project 8, whose capabilities answer is
	// movedCapabilities; project 7 no longer has the issue.
	moved             bool
	movedCapabilities string
	gone              bool // every issue read 404s, as after a soft delete
}

func (d *relatedDaemon) eventRequests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, r := range d.requests {
		if strings.Contains(r, "/events") {
			out = append(out, r)
		}
	}
	return out
}

func (d *relatedDaemon) handler(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.requests = append(d.requests, r.URL.RequestURI())
	moved, gone := d.moved, d.gone
	d.mu.Unlock()
	home := "/api/v1/projects/7"
	if moved {
		home = "/api/v1/projects/8"
	}
	w.Header().Set("Content-Type", "application/json")
	if gone && strings.Contains(r.URL.Path, "/issues/") {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case "/api/v1/projects/7/issues/issue-a":
		if moved {
			http.NotFound(w, r)
			return
		}
		corePeekHandler(w, r)
	case "/api/v1/issues/issue-a":
		if !moved {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"issue":{"uid":"issue-a","short_id":"ab12","title":"Moved issue","project_id":8},"comments":[` +
			`{"uid":"comment-a","author":"example-author","body":"protected comment marker","created_at":"2026-01-01T08:00:00Z"}]}`))
	case "/api/v1/projects/7/cron/capabilities":
		_, _ = w.Write([]byte(d.capabilities))
	case "/api/v1/projects/8/cron/capabilities":
		_, _ = w.Write([]byte(d.movedCapabilities))
	case home + "/events":
		if d.block != nil {
			close(d.block)
			<-r.Context().Done()
			return
		}
		if d.eventsStatus != 0 {
			w.WriteHeader(d.eventsStatus)
			_, _ = w.Write([]byte(`{"error":"events unavailable"}`))
			return
		}
		if d.ack != "-" {
			w.Header().Set("X-Kata-Events-Issue-UID", d.ack)
		}
		d.mu.Lock()
		page := d.pages[r.URL.Query().Get("after_id")]
		d.mu.Unlock()
		_, _ = w.Write([]byte(page))
	default:
		http.NotFound(w, r)
	}
}

func eventsPage(next int64, events ...EventLogEntry) string {
	body, _ := json.Marshal(map[string]any{"events": events, "next_after_id": next})
	return string(body)
}

func runAll(cmd tea.Cmd) []tea.Msg {
	out := make(chan tea.Msg, 32)
	coreExecute(cmd, out)
	close(out)
	var msgs []tea.Msg
	for msg := range out {
		msgs = append(msgs, msg)
	}
	return msgs
}

// deliver feeds a command's messages back into the host until it settles.
func deliver(h *cronHost, cmd tea.Cmd) {
	for _, msg := range runAll(cmd) {
		_, next := h.Update(msg)
		deliver(h, next)
	}
}

const enabledCapabilities = `{"project_uid":"project-uid","event_features":["cron"],"agentsview_enabled":true}`

func newRelatedDaemon() *relatedDaemon {
	return &relatedDaemon{
		capabilities: enabledCapabilities,
		ack:          "issue-a",
		pages:        map[string]string{"": eventsPage(1, closedEvent(1, "project-uid", "issue-a", transcriptPayload(relatedURL)))},
	}
}

// newRelatedHost returns a host whose peek for issue-a is ready, with the
// optional read either completed or, when a daemon blocks, in flight.
func newRelatedHost(t *testing.T, d *relatedDaemon, opened *[]string) (*cronHost, tea.Cmd) {
	t.Helper()
	h := newCorePeekHost(t, d.handler)
	if opened != nil {
		h.native.opts.OpenURL = func(_ context.Context, target string) error {
			*opened = append(*opened, target)
			return nil
		}
	}
	_, optional := h.Update(corePeekReply(t, h))
	if !strings.Contains(ansi.Strip(h.View().Content), "protected comment marker") {
		t.Fatal("ordinary peek is not readable")
	}
	return h, optional
}

func pressOpen(h *cronHost) {
	_, cmd := h.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	deliver(h, cmd)
}

func TestAgentsViewCronRelatedContext(t *testing.T) {
	t.Run("enabled safe link opens exact bytes", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		h, optional := newRelatedHost(t, d, &opened)
		deliver(h, optional)
		text := ansi.Strip(h.View().Content)
		if !strings.Contains(text, "Related issue session") || !strings.Contains(text, "o Open in AgentsView") || !strings.Contains(text, "protected comment marker") {
			t.Fatalf("enabled peek lacks the related session action:\n%s", text)
		}
		pressOpen(h)
		if len(opened) != 1 || opened[0] != relatedURL {
			t.Fatalf("opened %q, want exactly %q", opened, relatedURL)
		}
		if got := d.eventRequests(); len(got) != 1 || !strings.Contains(got[0], "issue=issue-a") || !strings.Contains(got[0], "limit=1000") {
			t.Fatalf("event requests = %q", got)
		}
	})
	for name, capabilities := range map[string]string{
		"disabled":      `{"project_uid":"project-uid","event_features":["cron"],"agentsview_enabled":false}`,
		"absent":        `{"project_uid":"project-uid","event_features":["cron"]}`,
		"other project": `{"project_uid":"other-project","event_features":["cron"],"agentsview_enabled":true}`,
		"decimal key":   `{"project_uid":"7","event_features":["cron"],"agentsview_enabled":true}`,
	} {
		t.Run(name+" gate makes no history read", func(t *testing.T) {
			d, opened := newRelatedDaemon(), []string{}
			d.capabilities = capabilities
			h, optional := newRelatedHost(t, d, &opened)
			deliver(h, optional)
			pressOpen(h)
			if text := ansi.Strip(h.View().Content); strings.Contains(text, "AgentsView") || strings.Contains(text, "Related issue session") {
				t.Fatalf("closed gate shows the action:\n%s", text)
			}
			if got := d.eventRequests(); len(got) != 0 || len(opened) != 0 {
				t.Fatalf("closed gate read %q and opened %q", got, opened)
			}
		})
	}
	t.Run("no browser adapter makes no optional read", func(t *testing.T) {
		d := newRelatedDaemon()
		h, optional := newRelatedHost(t, d, nil)
		deliver(h, optional)
		if optional != nil || len(d.requests) != 1 {
			t.Fatalf("optional reads without an opener: %q", d.requests)
		}
	})
	t.Run("one page only, safe url on page two is unavailable", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		d.pages = map[string]string{
			"":     eventsPage(1000, closedEvent(4, "project-uid", "issue-a", transcriptPayload("https://user@agentsview.example/x"))),
			"1000": eventsPage(1001, closedEvent(1001, "project-uid", "issue-a", transcriptPayload(relatedURL))),
		}
		h, optional := newRelatedHost(t, d, &opened)
		deliver(h, optional)
		pressOpen(h)
		if got := d.eventRequests(); len(got) != 1 || strings.Contains(got[0], "after_id") {
			t.Fatalf("event requests = %q, want exactly one first page", got)
		}
		if strings.Contains(ansi.Strip(h.View().Content), "AgentsView") || len(opened) != 0 {
			t.Fatal("a later page's locator became actionable")
		}
	})
	unavailable := map[string]func(d *relatedDaemon){
		"missing ack":    func(d *relatedDaemon) { d.ack = "-" },
		"mismatched ack": func(d *relatedDaemon) { d.ack = "issue-b" },
		"reset": func(d *relatedDaemon) {
			d.pages[""] = strings.Replace(d.pages[""], `{"events"`, `{"reset_required":true,"events"`, 1)
		},
		"unsafe url": func(d *relatedDaemon) {
			d.pages[""] = eventsPage(1, closedEvent(1, "project-uid", "issue-a", transcriptPayload("https://agentsview.example/x?token=secret")))
		},
		"no close": func(d *relatedDaemon) { d.pages[""] = eventsPage(0) },
		"404":      func(d *relatedDaemon) { d.eventsStatus = http.StatusNotFound },
		"403":      func(d *relatedDaemon) { d.eventsStatus = http.StatusForbidden },
		"500":      func(d *relatedDaemon) { d.eventsStatus = http.StatusInternalServerError },
	}
	for name, mutate := range unavailable {
		t.Run(name+" keeps the ordinary peek", func(t *testing.T) {
			d, opened := newRelatedDaemon(), []string{}
			mutate(d)
			h, optional := newRelatedHost(t, d, &opened)
			deliver(h, optional)
			pressOpen(h)
			text := ansi.Strip(h.View().Content)
			if strings.Contains(text, "AgentsView") || len(opened) != 0 {
				t.Fatalf("unavailable locator became actionable:\n%s", text)
			}
			if !strings.Contains(text, "protected comment marker") || !strings.Contains(text, "own summary marker") || h.native.activeAuthRejected || len(h.catalogs) == 0 {
				t.Fatalf("optional failure damaged the ordinary peek:\n%s", text)
			}
		})
	}
	t.Run("401 keeps baseline credential rejection", func(t *testing.T) {
		d := newRelatedDaemon()
		d.eventsStatus = http.StatusUnauthorized
		h, optional := newRelatedHost(t, d, &[]string{})
		deliver(h, optional)
		if !h.native.activeAuthRejected || len(h.catalogs) != 0 {
			t.Fatal("Kata 401 on the optional read did not reject the credential")
		}
	})
	t.Run("pending optional read does not delay the peek and close cancels it", func(t *testing.T) {
		d := newRelatedDaemon()
		d.block = make(chan struct{})
		h, optional := newRelatedHost(t, d, &[]string{})
		done := make(chan []tea.Msg, 1)
		go func() { done <- runAll(optional) }()
		select {
		case <-d.block:
		case <-time.After(2 * time.Second):
			t.Fatal("optional read did not start")
		}
		if !strings.Contains(ansi.Strip(h.View().Content), "protected comment marker") {
			t.Fatal("pending optional read hid the ordinary peek")
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case msgs := <-done:
			for _, msg := range msgs {
				h.Update(msg)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown retained the optional read")
		}
		if strings.Contains(ansi.Strip(h.View().Content), "AgentsView") {
			t.Fatal("late optional result shown after shutdown")
		}
	})
	for name, transition := range map[string]func(h *cronHost){
		"selection": func(h *cronHost) { h.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) },
		"authority": func(h *cronHost) { h.native.activeAuthRejected = true; h.Update(struct{}{}) },
		"daemon":    func(h *cronHost) { h.native.connGen++; h.Update(struct{}{}) },
		"project uid": func(h *cronHost) {
			cat := h.catalogs[7]
			cat.uid = "replacement-uid"
			h.catalogs[7] = cat
			h.Update(struct{}{})
		},
	} {
		t.Run(name+" switch discards late locator", func(t *testing.T) {
			d, opened := newRelatedDaemon(), []string{}
			h, optional := newRelatedHost(t, d, &opened)
			late := runAll(optional)
			transition(h)
			for _, msg := range late {
				h.Update(msg)
			}
			if name == "selection" {
				// Returning to the same run starts a new generation; the old
				// reply still must not attach to it.
				_, cmd := h.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
				for _, msg := range runAll(cmd) {
					if reply, ok := msg.(cronPeekMsg); ok {
						h.Update(reply)
					}
				}
				for _, msg := range late {
					h.Update(msg)
				}
			}
			pressOpen(h)
			if strings.Contains(ansi.Strip(h.View().Content), "AgentsView") || len(opened) != 0 {
				t.Fatalf("stale locator survived the %s switch", name)
			}
		})
	}
	t.Run("switch after delivery hides the action", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		h, optional := newRelatedHost(t, d, &opened)
		deliver(h, optional)
		h.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
		pressOpen(h)
		if strings.Contains(ansi.Strip(h.View().Content), "AgentsView") || len(opened) != 0 {
			t.Fatal("action followed the selection to another run")
		}
	})
}

// The host opens only the locator its current peek accepted.
func TestAgentsViewCronOpenRejectsOtherURL(t *testing.T) {
	d, opened := newRelatedDaemon(), []string{}
	h, optional := newRelatedHost(t, d, &opened)
	deliver(h, optional)
	deliver(h, h.updateCronChild(cronui.OpenRelatedMsg{URL: "https://agentsview.example/other"}))
	if len(opened) != 0 {
		t.Fatalf("opened a locator the peek never accepted: %q", opened)
	}
}

func TestAgentsViewCronOpenFailureIsReported(t *testing.T) {
	d := newRelatedDaemon()
	h, optional := newRelatedHost(t, d, nil)
	h.native.opts.OpenURL = func(context.Context, string) error { return fmt.Errorf("no browser") }
	deliver(h, optional)
	deliver(h, h.loadCronRelated(h.peekKey, h.peekGeneration, 0))
	pressOpen(h)
	if text := ansi.Strip(h.View().Content); !strings.Contains(text, "could not open AgentsView") || !strings.Contains(text, "protected comment marker") {
		t.Fatalf("browser failure not reported:\n%s", text)
	}
}

// issueEvent delivers a live event for issue-a and returns the refreshed peek
// reply without delivering it.
func issueEvent(t *testing.T, h *cronHost, eventType string) cronPeekMsg {
	t.Helper()
	_, cmd := h.Update(eventReceivedMsg{eventType: eventType, projectID: 7, projectUID: "project-uid", issueUID: "issue-a"})
	return corePeekReplyFrom(t, cmd)
}

func TestAgentsViewCronIssueEventRefresh(t *testing.T) {
	const newer = "https://agentsview.example/sessions/newer"
	t.Run("comment refresh keeps the locator without rereading it", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		h, optional := newRelatedHost(t, d, &opened)
		deliver(h, optional)
		_, related := h.Update(issueEvent(t, h, "issue.commented"))
		deliver(h, related)
		if related != nil || len(d.eventRequests()) != 1 {
			t.Fatalf("comment refresh reread history: %q", d.eventRequests())
		}
		pressOpen(h)
		if len(opened) != 1 || opened[0] != relatedURL {
			t.Fatalf("refresh lost the locator: opened %q", opened)
		}
	})
	t.Run("close refresh rereads the locator", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		h, optional := newRelatedHost(t, d, &opened)
		deliver(h, optional)
		d.mu.Lock()
		d.pages[""] = eventsPage(2, closedEvent(1, "project-uid", "issue-a", transcriptPayload(relatedURL)), closedEvent(2, "project-uid", "issue-a", transcriptPayload(newer)))
		d.mu.Unlock()
		_, related := h.Update(issueEvent(t, h, "issue.closed"))
		deliver(h, related)
		pressOpen(h)
		if got := d.eventRequests(); len(got) != 2 || len(opened) != 1 || opened[0] != newer {
			t.Fatalf("close refresh: requests %q, opened %q", got, opened)
		}
	})
	t.Run("refresh during a pending read reads again", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		h, optional := newRelatedHost(t, d, &opened)
		late := runAll(optional)
		_, related := h.Update(issueEvent(t, h, "issue.updated"))
		for _, msg := range late {
			h.Update(msg)
		}
		deliver(h, related)
		pressOpen(h)
		if len(d.eventRequests()) != 2 || len(opened) != 1 || opened[0] != relatedURL {
			t.Fatalf("discarded read was not replaced: requests %q, opened %q", d.eventRequests(), opened)
		}
	})
}

func TestAgentsViewCronMovedIssue(t *testing.T) {
	const newer = "https://agentsview.example/sessions/after-move"
	const movedCaps = `{"project_uid":"moved-project-uid","event_features":["cron"],"agentsview_enabled":true}`
	moved := func(d *relatedDaemon) {
		d.moved, d.movedCapabilities = true, movedCaps
		d.pages[""] = eventsPage(3,
			closedEvent(1, "project-uid", "issue-a", transcriptPayload(relatedURL)),
			closedEvent(3, "unrelated-project", "issue-a", transcriptPayload("https://agentsview.example/sessions/unrelated")))
	}
	t.Run("close recorded before the move", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		moved(d)
		h, optional := newRelatedHost(t, d, &opened)
		deliver(h, optional)
		pressOpen(h)
		if got := d.eventRequests(); len(got) != 1 || !strings.HasPrefix(got[0], "/api/v1/projects/8/events") || len(opened) != 1 || opened[0] != relatedURL {
			t.Fatalf("moved issue: requests %q, opened %q", got, opened)
		}
	})
	t.Run("close recorded after the move wins", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		moved(d)
		d.pages[""] = eventsPage(2,
			closedEvent(1, "project-uid", "issue-a", transcriptPayload(relatedURL)),
			closedEvent(2, "moved-project-uid", "issue-a", transcriptPayload(newer)))
		h, optional := newRelatedHost(t, d, &opened)
		deliver(h, optional)
		pressOpen(h)
		if len(opened) != 1 || opened[0] != newer {
			t.Fatalf("opened %q, want %q", opened, newer)
		}
	})
	for name, setup := range map[string]func(h *cronHost, d *relatedDaemon){
		"new project disabled": func(_ *cronHost, d *relatedDaemon) {
			d.movedCapabilities = `{"project_uid":"moved-project-uid","event_features":["cron"]}`
		},
		"catalog uid mismatch": func(h *cronHost, _ *relatedDaemon) {
			h.catalogs[8] = cronCatalog{name: "moved-project", uid: "other-uid"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, opened := newRelatedDaemon(), []string{}
			moved(d)
			h := newCorePeekHost(t, d.handler)
			h.native.opts.OpenURL = func(_ context.Context, target string) error { opened = append(opened, target); return nil }
			setup(h, d)
			_, optional := h.Update(corePeekReply(t, h))
			deliver(h, optional)
			pressOpen(h)
			if len(d.eventRequests()) != 0 || len(opened) != 0 {
				t.Fatalf("closed gate for the new project: requests %q, opened %q", d.eventRequests(), opened)
			}
		})
	}
	t.Run("catalog uid match", func(t *testing.T) {
		d, opened := newRelatedDaemon(), []string{}
		moved(d)
		h := newCorePeekHost(t, d.handler)
		h.native.opts.OpenURL = func(_ context.Context, target string) error { opened = append(opened, target); return nil }
		h.catalogs[8] = cronCatalog{name: "moved-project", uid: "moved-project-uid"}
		_, optional := h.Update(corePeekReply(t, h))
		deliver(h, optional)
		pressOpen(h)
		if len(opened) != 1 || opened[0] != relatedURL {
			t.Fatalf("opened %q", opened)
		}
	})
}

// Lifecycle events that can change which close is readable reread the
// locator; a soft delete voids it until the issue is readable again.
func TestAgentsViewCronLifecycleRefresh(t *testing.T) {
	const newer = "https://agentsview.example/sessions/newer"
	for _, eventType := range []string{"issue.moved", "issue.restored", "issue.snapshot"} {
		t.Run(eventType, func(t *testing.T) {
			d, opened := newRelatedDaemon(), []string{}
			h, optional := newRelatedHost(t, d, &opened)
			deliver(h, optional)
			d.mu.Lock()
			d.pages[""] = eventsPage(2, closedEvent(2, "project-uid", "issue-a", transcriptPayload(newer)))
			d.mu.Unlock()
			_, related := h.Update(issueEvent(t, h, eventType))
			deliver(h, related)
			pressOpen(h)
			if len(d.eventRequests()) != 2 || len(opened) != 1 || opened[0] != newer {
				t.Fatalf("%s: requests %q, opened %q", eventType, d.eventRequests(), opened)
			}
		})
	}
	// Back from unreadable, even an ordinary refresh rereads the session.
	for _, back := range []string{"issue.restored", "issue.updated"} {
		t.Run("soft delete then "+back, func(t *testing.T) {
			d, opened := newRelatedDaemon(), []string{}
			h, optional := newRelatedHost(t, d, &opened)
			deliver(h, optional)
			setGone := func(gone bool) {
				d.mu.Lock()
				d.gone = gone
				d.mu.Unlock()
			}
			setGone(true)
			_, related := h.Update(issueEvent(t, h, "issue.soft_deleted"))
			deliver(h, related)
			pressOpen(h)
			if text := ansi.Strip(h.View().Content); !strings.Contains(text, "Issue not readable") || strings.Contains(text, "AgentsView") || len(opened) != 0 {
				t.Fatalf("deleted issue kept its related session:\n%s", text)
			}
			setGone(false)
			_, related = h.Update(issueEvent(t, h, back))
			deliver(h, related)
			pressOpen(h)
			if len(d.eventRequests()) != 2 || len(opened) != 1 || opened[0] != relatedURL {
				t.Fatalf("restore did not bring the session back: requests %q, opened %q", d.eventRequests(), opened)
			}
		})
	}
}
