package tui

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

func cronNoticeFixture(t *testing.T) Model {
	t.Helper()
	m := cronSeenFixture(t)
	m.cronStartedAt = snapshotFixedNow
	m.toastNow = func() time.Time { return snapshotFixedNow }
	return m
}
func cronNoticeEvent(uid, job string, at time.Time) eventReceivedMsg {
	return eventReceivedMsg{eventType: "cron.run.observed", projectID: 7, projectUID: "project-example", cronRun: &cronRunEvent{UID: uid, JobUID: job, ProjectUID: "project-example", Status: "failed", UpdatedAt: at}}
}
func cronNoticeTickFor(m Model) cronNoticeTickMsg {
	return cronNoticeTickMsg{conn: m.connGen, scope: m.scopeGen, gen: m.cronNotices.gen}
}
func TestCronNoticeDecodeObservedOnly(t *testing.T) {
	payload := map[string]any{"uid": "run", "project_uid": "project-example", "status": "unknown", "job_uid": "job", "updated_at": snapshotFixedNow}
	for _, kind := range []string{"cron.run.observed", "cron.run.snapshot", "issue.updated"} {
		raw, err := json.Marshal(map[string]any{"type": kind, "project_id": 7, "project_uid": "project-example", "payload": payload})
		require.NoError(t, err)
		msg := decodeEventReceived(frame{data: raw})
		if kind == "cron.run.observed" {
			require.NotNil(t, msg.cronRun)
			require.Equal(t, "run", msg.cronRun.UID)
		} else {
			require.Nil(t, msg.cronRun)
		}
	}
	for _, raw := range []string{`{"type":"cron.run.observed"}`, `{"type":"cron.run.observed","payload":{"uid":"run","updated_at":"bad"}}`, `{"type":"cron.run.observed","payload":42}`} {
		require.Nil(t, decodeEventReceived(frame{data: []byte(raw)}).cronRun)
	}
}

// Contract: permutations/replays within a window count distinct eligible run
// UIDs once. Same-job failures count individually, but get one label.
func TestCronNoticeWindowPermutation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		m := cronNoticeFixture(t)
		n := int(hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))%8) + 1
		offset := int(hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0))) % uint64(n))
		for i := range n * 2 {
			next, cmd := m.cronNoticeEvent(cronNoticeEvent(fmt.Sprint((i+offset)%n), "job", snapshotFixedNow))
			m = next
			if (cmd != nil) != (i == 0) {
				ht.Fatalf("event %d armed an unexpected window", i)
			}
		}
		m, cmd := m.cronNoticeFlush(cronNoticeTickFor(m))
		if cmd == nil || m.toast == nil {
			ht.Fatal("eligible window must emit a toast")
		}
		if !strings.Contains(m.toast.text, fmt.Sprintf("%d run", n)) || strings.Count(m.toast.text, "job job") != 1 {
			ht.Fatalf("wrong grouped notice: %q", m.toast.text)
		}
		if !m.toast.expiresAt.Equal(snapshotFixedNow.Add(10 * time.Second)) {
			ht.Fatal("wrong notice lifetime")
		}
		before := m.toast
		m, cmd = m.cronNoticeEvent(cronNoticeEvent("next", "job", snapshotFixedNow))
		if cmd != nil || m.toast != before {
			ht.Fatal("an unseen announcement must suppress a later window")
		}
	}, hegel.WithTestCases(200))
}
func TestCronNoticeSeenBeforeFlushAndJobRelease(t *testing.T) {
	m := cronNoticeFixture(t)
	for _, id := range []string{"a", "b"} {
		m, _ = m.cronNoticeEvent(cronNoticeEvent(id, "job", snapshotFixedNow))
	}
	m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a", "b"}}
	m, cmd := m.cronNoticeFlush(cronNoticeTickFor(m))
	require.Nil(t, cmd)
	require.Nil(t, m.toast)
	// A job is not released until all of its emitted run failures are seen.
	m = cronNoticeFixture(t)
	for _, id := range []string{"a", "b"} {
		m, _ = m.cronNoticeEvent(cronNoticeEvent(id, "job", snapshotFixedNow))
	}
	m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
	m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a"}}
	m, cmd = m.cronNoticeEvent(cronNoticeEvent("c", "job", snapshotFixedNow))
	require.Nil(t, cmd)
	m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a", "b"}}
	m, cmd = m.cronNoticeEvent(cronNoticeEvent("c", "job", snapshotFixedNow))
	require.NotNil(t, cmd)
	m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
	m, cmd = m.cronNoticeEvent(cronNoticeEvent("c", "job", snapshotFixedNow))
	require.Nil(t, cmd, "same-UID replay stays suppressed even after acknowledgement")
}
func TestCronNoticeStartupScopeAndCutoff(t *testing.T) {
	m := cronNoticeFixture(t)
	m.cronSeen.ready = false
	m, cmd := m.cronNoticeEvent(cronNoticeEvent("early", "job", snapshotFixedNow))
	require.Nil(t, cmd)
	require.Nil(t, m.toast)
	m, cmd = m.cronSeenResult(cronSeenResultMsg{data: cronSeenStore{Version: 1}})
	require.NotNil(t, cmd, "an event before first Cron entry arms after startup load")
	tick := cronNoticeTickFor(m)
	m.connGen++
	m, _ = m.cronNoticeFlush(tick)
	require.Nil(t, m.toast)
	m = cronNoticeFixture(t)
	m.cronSeen.ready = false
	m, _ = m.cronNoticeEvent(cronNoticeEvent("early", "job", snapshotFixedNow))
	m, cmd = m.cronSeenResult(cronSeenResultMsg{data: cronSeenStore{Version: 1, Projects: map[string]cronSeenProject{"project-example": {Runs: []string{"early"}}}}})
	require.Nil(t, cmd, "persisted acknowledgement must not arm a notice window")
	require.False(t, m.cronRunUnseen("project-example", cronNoticeEvent("early", "job", snapshotFixedNow).cronRun.run()))
	require.Nil(t, m.toast, "persisted seen state wins over buffered event")
	for _, tc := range []struct {
		name string
		edit func(*Model, *eventReceivedMsg)
	}{
		{"old", func(m *Model, e *eventReceivedMsg) {
			e.cronRun.UpdatedAt = m.cronStartedAt.Add(-2*time.Minute - time.Nanosecond)
		}},
		{"all projects", func(m *Model, _ *eventReceivedMsg) { m.scope.allProjects = true }},
		{"foreign payload", func(_ *Model, e *eventReceivedMsg) { e.cronRun.ProjectUID = "other-project" }},
		{"foreign event", func(_ *Model, e *eventReceivedMsg) { e.projectID = 8 }},
		{"missing time", func(_ *Model, e *eventReceivedMsg) { e.cronRun.UpdatedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cronNoticeFixture(t)
			e := cronNoticeEvent("run", "job", snapshotFixedNow)
			tc.edit(&m, &e)
			m, cmd := m.cronNoticeEvent(e)
			require.Nil(t, cmd)
			require.Empty(t, m.cronNotices.pending)
		})
	}
	m = cronNoticeFixture(t)
	m, cmd = m.cronNoticeEvent(cronNoticeEvent("edge", "job", snapshotFixedNow.Add(-2*time.Minute)))
	require.NotNil(t, cmd, "cutoff equality is eligible")
	// Workflow-only observations do not share an empty-job suppression bucket.
	m = cronNoticeFixture(t)
	m, _ = m.cronNoticeEvent(cronNoticeEvent("first", "", snapshotFixedNow))
	m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
	m, cmd = m.cronNoticeEvent(cronNoticeEvent("second", "", snapshotFixedNow))
	require.NotNil(t, cmd)
}
func TestCronNoticeStaleWindowAndHints(t *testing.T) {
	m := cronNoticeFixture(t)
	m, _ = m.cronNoticeEvent(cronNoticeEvent("a", "job", snapshotFixedNow))
	old := cronNoticeTickFor(m)
	m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a"}}
	m, _ = m.cronNoticeFlush(old)
	m, _ = m.cronNoticeEvent(cronNoticeEvent("b", "job", snapshotFixedNow))
	m, _ = m.cronNoticeFlush(old)
	require.Nil(t, m.toast, "old tick must not flush a newer window")
	m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
	require.NotNil(t, m.toast)
	for _, tc := range []struct {
		view viewID
		want string
	}{{viewCron, "u to review"}, {viewList, "3 then u"}, {viewDetail, "3 then u"}, {viewProjects, "open Cron Runs"}} {
		m.view = tc.view
		require.Contains(t, m.cronToast().text, tc.want)
	}
	m.view = viewCron
	m.cron.filtering = true
	require.Contains(t, m.cronToast().text, "open Cron Runs")
	// Real SSE dispatch in the issue list invokes the same notice path.
	m = cronNoticeFixture(t)
	m.view = viewList
	next, _ := m.handleEventReceived(cronNoticeEvent("r", "job", snapshotFixedNow))
	m = next.(Model)
	require.Len(t, m.cronNotices.pending, 1)

	{
		defer snapshotInit(t)()
		m := cronNoticeFixture(t)
		m.width = 120
		m, _ = m.cronNoticeEvent(cronNoticeEvent("run-a", m.cron.data.jobs[0].UID, snapshotFixedNow))
		m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
		assertGolden(t, "cron-notice", m.viewContent())
		m.view = viewList
		assertGolden(t, "cron-notice-list", m.viewContent())
	}
}

func TestCronNoticeShortcutEnablesUnseen(t *testing.T) {
	m := cronNoticeFixture(t)
	m.cron.unseenOnly = true
	m.toast = &toast{cronFailure: true, expiresAt: snapshotFixedNow.Add(time.Minute)}
	m, _ = m.routeCronKey(keyRune('u'))
	require.True(t, m.cron.unseenOnly)
	m.toast = nil
	m, _ = m.routeCronKey(keyRune('u'))
	require.False(t, m.cron.unseenOnly, "ordinary u keeps its toggle behavior")
}

func TestCronNoticeCorrectionReleasesSuppression(t *testing.T) {
	for _, announced := range []bool{false, true} {
		m := cronNoticeFixture(t)
		m, _ = m.cronNoticeEvent(cronNoticeEvent("corrected", "job", snapshotFixedNow))
		if announced {
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			m.toast = nil
		}
		corrected := cronNoticeEvent("corrected", "job", snapshotFixedNow)
		corrected.cronRun.Status = "succeeded"
		m, _ = m.cronNoticeEvent(corrected)
		require.NotContains(t, m.cronNotices.pending, "corrected")
		require.NotContains(t, m.cronNotices.announced, "corrected")
		if !announced {
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			require.Nil(t, m.toast)
		}
		m, cmd := m.cronNoticeEvent(cronNoticeEvent("next", "job", snapshotFixedNow))
		require.NotNil(t, cmd, "corrected failure must release job suppression")
	}

	t.Run("loaded correction after reset", func(t *testing.T) {
		m := cronNoticeFixture(t)
		job := m.cron.data.jobs[0].UID
		event := cronNoticeEvent("corrected-run", job, snapshotFixedNow)
		m, _ = m.cronNoticeEvent(event)
		m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
		require.NotNil(t, m.toast)
		m.toast = nil
		current := m.cron.data.runs[0]
		current.UID, current.Status, current.Revision = "corrected-run", "succeeded", 2
		data := *m.cron.data
		data.runs = []generated.CronRun{current}
		data.lastRuns = map[string]generated.CronRun{job: current}
		data.lookup = map[string]generated.CronRun{current.UID: current}
		data.resetPages = true
		m = m.cronLoaded(cronLoadedMsg(data))
		require.Zero(t, m.cronUnseenCount())
		m, cmd := m.cronNoticeEvent(cronNoticeEvent("new-failure", job, snapshotFixedNow))
		require.NotNil(t, cmd, "a corrected failure is no longer an unseen failure of this job")
		m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
		require.NotNil(t, m.toast)
	})
}

// Failure notices must use error styling and leave the help bar at the bottom.
// Drive SSE dispatch and the window tick, rather than assigning a toast.
func TestCronNoticeRenderedFailure(t *testing.T) {
	defer snapshotInit(t)()
	m := cronNoticeFixture(t)
	applyColorMode(colorDark, true)
	m.width, m.height = 120, 26
	baseline := strings.Split(stripANSI(m.viewContent()), "\n")
	event := cronNoticeEvent("new-failure", m.cron.data.jobs[0].UID, snapshotFixedNow)
	m, cmd := updateModel(m, event)
	require.NotNil(t, cmd)
	require.True(t, m.pendingRefetch, "SSE must schedule fresh data")
	m, _ = updateModel(m, cronNoticeTickFor(m))
	require.NotNil(t, m.toast)
	require.Equal(t, toastError, m.toast.level)
	lines := strings.Split(m.viewContent(), "\n")
	notice := -1
	for i, line := range lines {
		if strings.Contains(stripANSI(line), "cron: 1 run failed:") {
			notice = i
		}
	}
	require.NotEqual(t, -1, notice)
	t.Run("severity", func(t *testing.T) {
		want := attentionTerminalCells(errorStyle.Render("x"))[0].foreground
		x := strings.Index(stripANSI(lines[notice]), "cron:")
		cell := attentionTerminalCells(lines[notice])[x]
		require.Equal(t, want, cell.foreground, "failure notice must use the error foreground")
	})
	t.Run("footer", func(t *testing.T) {
		require.Equal(t, baseline[len(baseline)-1], stripANSI(lines[len(lines)-1]), "notice must precede the help bar")
		require.Less(t, notice, len(lines)-1)
	})

	// The old success can remain visible while the asynchronous read is pending.
	// Deliver its fresh result: no extra event-projection mechanism is needed.
	m, _ = updateModel(m, refetchTickMsg{})
	data := *m.cron.data
	data.events, data.generation = m.cron.events, m.cron.readGen
	run := data.runs[0]
	run.UID, run.Status, run.Revision = "new-failure", "failed", 1
	run.CreatedAt, run.UpdatedAt, run.EndedAt = snapshotFixedNow, snapshotFixedNow, new(snapshotFixedNow)
	data.runs = []generated.CronRun{run}
	data.lastRuns = map[string]generated.CronRun{*run.JobUID: run}
	m, _ = updateModel(m, cronLoadedMsg(data))
	out := stripANSI(m.viewContent())
	require.Contains(t, out, "1 unseen failed")
	require.Contains(t, out, "failing")
	require.Contains(t, out, "failed 0s ago")
}

// Failure severity applies to the shared notice row outside the Cron list.
func TestCronNoticeProjectsFailureSeverity(t *testing.T) {
	defer snapshotInit(t)()
	m := cronNoticeFixture(t)
	applyColorMode(colorDark, true)
	m.width, m.height = 120, 26
	m.view = viewProjects
	m, _ = updateModel(m, cronNoticeEvent("failure", "job", snapshotFixedNow))
	m, _ = updateModel(m, cronNoticeTickFor(m))
	for _, line := range strings.Split(m.viewContent(), "\n") {
		plain := stripANSI(line)
		if x := strings.Index(plain, "cron: 1 run failed:"); x >= 0 {
			cell := attentionTerminalCells(line)[ansi.StringWidth(plain[:x])]
			want := attentionTerminalCells(errorStyle.Render("x"))[0].foreground
			require.Equal(t, want, cell.foreground, "Projects failure notice must use the error foreground")
			return
		}
	}
	t.Fatal("failure notice missing from Projects")
}

// An emitted failure belongs to the scope and connection that observed it.
func TestCronNoticeClearsOnSwitch(t *testing.T) {
	for _, target := range []string{"project", "all projects", "daemon"} {
		t.Run(target, func(t *testing.T) {
			m := cronNoticeFixture(t)
			m, _ = updateModel(m, cronNoticeEvent("failure", "job", snapshotFixedNow))
			m, _ = updateModel(m, cronNoticeTickFor(m))
			require.NotNil(t, m.toast)
			switch target {
			case "project", "all projects":
				m.projectsCursor = 0
				m, _ = m.applyProjectsViewSelection([]projectsRow{{projectID: 8, name: "other-project", sentinel: target == "all projects"}})
				m, _ = updateModel(m, cronNoticeTickFor(m))
			case "daemon":
				m, _ = updateModel(m, daemonSwitchResultMsg{conn: daemonConnection{
					api: NewClient("http://daemon.example", nil),
					init: bootInit{view: viewList, scope: homedScope(8, "other-project")},
				}})
			}
			require.Nil(t, m.toast, "departed scope's failure must not survive a switch")
			m.view = viewCron
			m.cron.unseenOnly = true
			m, _ = m.routeCronKey(keyRune('u'))
			require.False(t, m.cron.unseenOnly, "u must toggle normally after switching")
		})
	}
}
