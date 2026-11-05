package tui

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
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
		n := hegel.Draw(ht, hegel.Integers[int](1, 8))
		m.width = hegel.Draw(ht, hegel.Integers[int](64, 160))
		jobs := []string{"named-a", "named-b", "0123456789abcdef"}
		m.cron.data.jobs = []generated.CronJob{{UID: jobs[0], Name: "alpha\tjob"}, {UID: jobs[1], Name: "beta"}}
		labels := []string{"alpha job", "beta", "job 0123456789a…"}
		chosen := make([]string, n)
		wantLabels := []string{}
		for i := range n {
			j := hegel.Draw(ht, hegel.Integers[int](0, len(jobs)-1))
			chosen[i] = jobs[j]
			if !slices.Contains(wantLabels, labels[j]) {
				wantLabels = append(wantLabels, labels[j])
			}
		}
		slices.Sort(wantLabels)
		offset := hegel.Draw(ht, hegel.Integers[int](0, n-1))
		for i := range n * 2 {
			next, cmd := m.cronNoticeEvent(cronNoticeEvent(fmt.Sprint((i+offset)%n), chosen[(i+offset)%n], snapshotFixedNow))
			m = next
			if (cmd != nil) != (i == 0) {
				ht.Fatalf("event %d armed an unexpected window", i)
			}
		}
		m, cmd := m.cronNoticeFlush(cronNoticeTickFor(m))
		if cmd == nil || m.toast == nil {
			ht.Fatal("eligible window must emit a toast")
		}
		wantText := strings.Join(wantLabels[:min(2, len(wantLabels))], ", ")
		if len(wantLabels) > 2 {
			wantText += fmt.Sprintf(" +%d", len(wantLabels)-2)
		}
		// Fixture labels are ASCII except the one-column ellipsis.
		if limit := max(m.width-52, 12); len([]rune(wantText)) > limit {
			wantText = string([]rune(wantText)[:limit-1]) + "…"
		}
		count := fmt.Sprintf("%d runs", n)
		if n == 1 {
			count = "1 run"
		}
		if want := "cron: " + count + " failed: " + wantText; m.toast.text != want {
			ht.Fatalf("grouped notice = %q, want %q", m.toast.text, want)
		}
		if !m.toast.expiresAt.Equal(snapshotFixedNow.Add(10 * time.Second)) {
			ht.Fatal("wrong notice lifetime")
		}
	}, hegel.WithTestCases(200))
}
func TestCronNoticeSeenBeforeFlush(t *testing.T) {
	m := cronNoticeFixture(t)
	for _, id := range []string{"a", "b"} {
		m, _ = m.cronNoticeEvent(cronNoticeEvent(id, "job", snapshotFixedNow))
	}
	m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a", "b"}}
	m, cmd := m.cronNoticeFlush(cronNoticeTickFor(m))
	require.Nil(t, cmd)
	require.Nil(t, m.toast)
}
func TestCronNoticeStartupScopeAndCutoff(t *testing.T) {
	m := cronNoticeFixture(t)
	m.cronSeen.ready = false
	m, cmd := m.cronNoticeEvent(cronNoticeEvent("early", "job", snapshotFixedNow))
	require.Nil(t, cmd)
	require.Nil(t, m.toast)
	m, cmd = m.cronSeenResult(cronSeenResultMsg{data: cronSeenStore{Version: 1}})
	require.NotNil(t, cmd, "an event before first Cron entry arms after startup load")
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
	m, cmd = m.cronNoticeEvent(cronNoticeEvent("first", "", snapshotFixedNow))
	require.Nil(t, cmd, "same-UID workflow replay must stay suppressed without a job bucket")
	require.Empty(t, m.cronNotices.pending)
	m, cmd = m.cronNoticeEvent(cronNoticeEvent("second", "", snapshotFixedNow))
	require.NotNil(t, cmd)
}
func TestCronNoticeStaleWindowAndHints(t *testing.T) {
	for _, fence := range []string{"connection", "scope", "generation"} {
		t.Run(fence, func(t *testing.T) {
			m := cronNoticeFixture(t)
			m.cronSeen.ready = false
			m, _ = m.cronNoticeEvent(cronNoticeEvent("early", "job", snapshotFixedNow))
			m, _ = m.cronSeenResult(cronSeenResultMsg{data: cronSeenStore{Version: 1}})
			tick := cronNoticeTickFor(m)
			switch fence {
			case "connection":
				m.connGen++
			case "scope":
				m.scopeGen++
			case "generation":
				m.cronNotices.gen++
			}
			m, _ = m.cronNoticeFlush(tick)
			require.Nil(t, m.toast, "stale tick must not emit a notice")
		})
	}

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
		help bool
		want string
	}{{viewCron, false, "u to review"}, {viewCron, true, "open Cron Runs"}, {viewList, false, "3 then u"}, {viewDetail, false, "3 then u"}, {viewProjects, false, "open Cron Runs"}} {
		m.view, m.cron.help = tc.view, tc.help
		require.Contains(t, m.cronToast().text, tc.want)
	}
	m.view = viewCron
	m.cron.filtering = true
	require.Contains(t, m.cronToast().text, "open Cron Runs")

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
		plain := stripANSI(lines[notice])
		before, _, ok := strings.Cut(plain, "cron:")
		require.True(t, ok)
		cell := attentionTerminalCells(lines[notice])[ansi.StringWidth(before)]
		require.Equal(t, want, cell.foreground, "failure notice must use the error foreground")
	})
	t.Run("footer", func(t *testing.T) {
		require.Equal(t, baseline[len(baseline)-1], stripANSI(lines[len(lines)-1]), "notice must precede the help bar")
		require.Less(t, notice, len(lines)-1)
	})

	t.Run("projects", func(t *testing.T) {
		defer snapshotInit(t)()
		m := cronNoticeFixture(t)
		applyColorMode(colorDark, true)
		m.width, m.height = 120, 26
		m.view = viewProjects
		m, _ = updateModel(m, cronNoticeEvent("failure", "job", snapshotFixedNow))
		m, _ = updateModel(m, cronNoticeTickFor(m))
		for line := range strings.SplitSeq(m.viewContent(), "\n") {
			plain := stripANSI(line)
			if before, _, ok := strings.Cut(plain, "cron: 1 run failed:"); ok {
				cell := attentionTerminalCells(line)[ansi.StringWidth(before)]
				want := attentionTerminalCells(errorStyle.Render("x"))[0].foreground
				require.Equal(t, want, cell.foreground, "Projects failure notice must use the error foreground")
				return
			}
		}
		t.Fatal("failure notice missing from Projects")
	})

	for _, state := range []string{"help", "form", "narrow"} {
		t.Run(state, func(t *testing.T) {
			m := m
			switch state {
			case "help":
				m.cron.help = true
			case "form":
				m.cron.form = newNativeForm(t)
			case "narrow":
				m.width = 79
			}
			require.Equal(t, 1, strings.Count(stripANSI(m.viewContent()), "cron: 1 run failed:"), "notice must appear exactly once")
		})
	}

	t.Run("fresh failure after notice", func(t *testing.T) {
		defer snapshotInit(t)()
		m := cronNoticeFixture(t)
		m.width, m.height = 120, 26
		event := cronNoticeEvent("new-failure", m.cron.data.jobs[0].UID, snapshotFixedNow)
		m, cmd := updateModel(m, event)
		require.NotNil(t, cmd)
		require.True(t, m.pendingRefetch)
		m, _ = updateModel(m, cronNoticeTickFor(m))
		require.NotNil(t, m.toast)

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
	})
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
					api:  NewClient("http://daemon.example", nil),
					init: bootInit{view: viewList, scope: homedScope(8, "other-project")},
				}})
			}
			require.Nil(t, m.toast, "departed scope's failure must not survive a switch")
		})
	}
}

// The store, not the client, resolves concurrent observations of one run, by
// observation HLC. A failure event that loses that fold, whatever its peer
// revision or updated_at, must neither be announced once a read issued after
// it shows the store's row, nor keep suppressing its job's later failures.

// An announcement stops suppressing its job only after acknowledgement or
// authoritative correction. Peer revisions and timestamps are not local order.
func TestCronNoticeJobSuppressionRelease(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, Model)
	}{
		{"snapshot at arrival", func(t *testing.T, m Model) {
			m.cron.data.lookup = map[string]generated.CronRun{"old": {UID: "old", Status: "succeeded", Revision: 50, UpdatedAt: snapshotFixedNow}}
			first := cronNoticeEvent("old", "job", snapshotFixedNow)
			first.cronRun.Revision = 1
			m = cronUpdateTest(m, first) // the real handler marks the run changed until a later read
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			m, cmd := m.cronNoticeEvent(cronNoticeEvent("next", "job", snapshotFixedNow))
			require.Nil(t, cmd, "the pre-event local snapshot cannot supersede the announcement")
			require.NotContains(t, m.cronNotices.pending, "next")
		}},
		{"all emitted failures are seen", func(t *testing.T, m Model) {
			// A job is not released until all of its emitted run failures are seen.
			for _, id := range []string{"a", "b"} {
				m, _ = m.cronNoticeEvent(cronNoticeEvent(id, "job", snapshotFixedNow))
			}
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a"}}
			m, cmd := m.cronNoticeEvent(cronNoticeEvent("c", "job", snapshotFixedNow))
			require.Nil(t, cmd)
			m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a", "b"}}
			m, cmd = m.cronNoticeEvent(cronNoticeEvent("c", "job", snapshotFixedNow))
			require.NotNil(t, cmd)
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
		}},
		{"loaded correction after reset", func(t *testing.T, m Model) {
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
		}},
	}
	T := snapshotFixedNow
	for _, at := range []struct {
		name string
		at   time.Time
	}{{"older", T}, {"equal", T.Add(time.Minute)}, {"skewed later", T.Add(2 * time.Minute)}} {
		for _, order := range []string{"read before flush", "flush before read", "winning failure"} {
			tests = append(tests, struct {
				name string
				run  func(*testing.T, Model)
			}{at.name + "/" + order, func(t *testing.T, m Model) {
				job := m.cron.data.jobs[0].UID
				loaded := generated.CronRun{UID: "old", JobUID: &job, Status: "succeeded", Revision: 5, CreatedAt: T, UpdatedAt: T.Add(time.Minute), EndedAt: new(T.Add(time.Minute))}
				m.cron.data.runs = append(m.cron.data.runs, loaded)
				event := cronNoticeEvent("old", job, at.at)
				event.cronRun.Revision = 9 // a source peer's revision, incomparable with local revision 5
				next, _ := m.handleEventReceived(event)
				m = next.(Model)
				require.Contains(t, m.cronNotices.pending, "old", "before any later read, the event is the only evidence")
				if order == "winning failure" {
					// The event won: the store's row became failed at local revision 6.
					won := loaded
					won.Status, won.Revision, won.UpdatedAt, won.EndedAt = "failed", 6, at.at, new(at.at)
					m.cron.data.runs[len(m.cron.data.runs)-1] = won
				}
				if order != "flush before read" {
					m = cronSettleTest(m)
					require.NotContains(t, m.cron.changed, "old")
				}
				m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
				if order == "read before flush" {
					require.Nil(t, m.toast, "the store kept the success; the losing failure is not announced")
					require.NotContains(t, m.cronNotices.announced, "old")
				} else {
					require.NotNil(t, m.toast)
					require.Contains(t, m.cronNotices.announced, "old")
				}
				if order == "flush before read" {
					m = cronSettleTest(m)
					require.NotContains(t, m.cron.changed, "old")
				}
				next, _ = m.handleEventReceived(cronNoticeEvent("next", job, T.Add(3*time.Minute)))
				m = next.(Model)
				if order == "winning failure" {
					require.NotContains(t, m.cronNotices.pending, "next", "an unseen winning failure still suppresses its job")
					return
				}
				require.Contains(t, m.cronNotices.pending, "next", "a losing failure must release its job")
				require.True(t, m.cronNotices.window)
				m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
				require.NotNil(t, m.toast)
				require.Contains(t, m.toast.text, "1 run")
				require.Contains(t, m.cronNotices.announced, "next")
				r, _ := m.cron.data.run("old")
				require.False(t, m.cronSeen.session[m.cronProjectUID()][r.UID], "releasing a job must not mark rows seen")
			}})
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, cronNoticeFixture(t))
		})
	}
}

// The store folds a run's observations by an HLC the client never sees, and
// a refresh issued before an event cannot describe it. A notice therefore
// trusts the store's row only from a read issued after the run's latest event:
// an older in-flight read cannot drop a newer failure, and a correction that
// may have lost the fold neither cancels nor releases a failure before then.
func TestCronNoticeWaitsForReadAfterEvent(t *testing.T) {
	T := snapshotFixedNow
	deliver := func(m Model, uid, status string, at time.Time) Model {
		msg := cronNoticeEvent(uid, m.cron.data.jobs[0].UID, at)
		msg.cronRun.Status = status
		next, _ := m.handleEventReceived(msg)
		return next.(Model)
	}
	// land answers a fetch that captured events with the store's row r.
	land := func(m Model, events uint64, r generated.CronRun) Model {
		p := *m.cron.data
		p.events, p.generation = events, m.cron.readGen
		p.runs = append(slices.DeleteFunc(slices.Clone(p.runs), func(x generated.CronRun) bool { return x.UID == r.UID }), r)
		m, _ = updateModel(m, cronLoadedMsg(p))
		return m
	}
	row := func(m Model, status string, revision int64, at time.Time) generated.CronRun {
		job := m.cron.data.jobs[0].UID
		r := generated.CronRun{UID: "r", JobUID: &job, Status: status, Revision: revision, CreatedAt: T, UpdatedAt: at}
		if status != "running" {
			r.EndedAt = new(at)
		}
		return r
	}
	for _, mode := range []string{"loaded", "unloaded", "exact read failed"} {
		t.Run("older read lands after failure/"+mode, func(t *testing.T) {
			m := cronNoticeFixture(t)
			if mode != "unloaded" {
				m = land(m, m.cron.events, row(m, "running", 5, T))
			}
			m = deliver(m, "r", "running", T.Add(time.Second)) // the run's start or progress
			inFlight := m.cron.events
			m = deliver(m, "r", "failed", T.Add(time.Minute))
			m = land(m, inFlight, row(m, "running", 6, T.Add(time.Second)))
			if mode == "exact read failed" {
				p := *m.cron.data
				p.events, p.generation = m.cron.events, m.cron.readGen
				p.runs = slices.DeleteFunc(slices.Clone(p.runs), func(r generated.CronRun) bool { return r.UID == "r" })
				p.runErr = map[string]error{"r": errors.New("run refresh offline")}
				m, _ = updateModel(m, cronLoadedMsg(p))
				require.NotZero(t, m.cron.changed["r"].at, "a failed exact read cannot settle the failure")
			}
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			require.NotNil(t, m.toast, "a read issued before the failure cannot supersede it")
			require.Contains(t, m.cronNotices.announced, "r")
		})
	}
	for _, wins := range []bool{false, true} {
		for _, flushed := range []bool{false, true} {
			t.Run(fmt.Sprintf("correction wins=%v/after flush=%v", wins, flushed), func(t *testing.T) {
				m := cronNoticeFixture(t)
				m = land(m, m.cron.events, row(m, "running", 5, T))
				m = deliver(m, "r", "failed", T.Add(time.Minute))
				failed := row(m, "failed", 6, T.Add(time.Minute))
				m = land(m, m.cron.events, failed)
				if flushed {
					m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
					require.NotNil(t, m.toast)
					m.toast = nil
				}
				m = deliver(m, "r", "succeeded", T.Add(30*time.Second))
				_, pending := m.cronNotices.pending["r"]
				_, announced := m.cronNotices.announced["r"]
				require.True(t, pending || announced, "the event alone does not settle the store's fold")
				store := failed
				if wins {
					store = row(m, "succeeded", 7, T.Add(30*time.Second))
				}
				m = land(m, m.cron.events, store)
				if !flushed {
					m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
					require.Equal(t, !wins, m.toast != nil, "the read after the correction decides the notice")
				}
				m = deliver(m, "next", "failed", T.Add(2*time.Minute))
				require.Equal(t, wins, m.cronNotices.window, "only a correction the store kept releases the job")
			})
		}
	}
}
