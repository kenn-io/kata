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
	payload := map[string]any{"uid": "run", "project_uid": "project-example", "status": "unknown", "job_uid": "job", "updated_at": snapshotFixedNow, "ended_at": snapshotFixedNow.Add(-time.Minute), "revision": int64(7)}
	for _, kind := range []string{"cron.run.observed", "cron.run.snapshot", "issue.updated"} {
		raw, err := json.Marshal(map[string]any{"type": kind, "project_id": 7, "project_uid": "project-example", "payload": payload})
		require.NoError(t, err)
		msg := decodeEventReceived(frame{data: raw})
		if kind == "cron.run.observed" {
			require.Equal(t, &cronRunEvent{
				UID: "run", JobUID: "job", ProjectUID: "project-example", Status: "unknown",
				Revision: 7, UpdatedAt: snapshotFixedNow, EndedAt: new(snapshotFixedNow.Add(-time.Minute)),
			}, msg.cronRun)
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
		order := make([]int, n*2)
		for i := range order {
			order[i] = i % n
		}
		for i := len(order) - 1; i > 0; i-- {
			j := hegel.Draw(ht, hegel.Integers[int](0, i))
			order[i], order[j] = order[j], order[i]
		}
		for i, id := range order {
			next, cmd := m.cronNoticeEvent(cronNoticeEvent(fmt.Sprint(id), chosen[id], snapshotFixedNow))
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
}
func TestCronNoticeStaleWindowAndHints(t *testing.T) {
	t.Run("window fences", func(t *testing.T) {
		for _, fence := range []string{"connection", "scope"} {
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
	})
	t.Run("hints", func(t *testing.T) {
		m := cronNoticeFixture(t)
		m, _ = m.cronNoticeEvent(cronNoticeEvent("b", "job", snapshotFixedNow))
		m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))

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

	t.Run("goldens", func(t *testing.T) {
		defer snapshotInit(t)()
		m := cronNoticeFixture(t)
		m.width = 120
		m, _ = m.cronNoticeEvent(cronNoticeEvent("run-a", m.cron.data.jobs[0].UID, snapshotFixedNow))
		m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
		assertGolden(t, "cron-notice", m.viewContent())
		m.view = viewList
		assertGolden(t, "cron-notice-list", m.viewContent())
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
//
// An announcement stops suppressing its job only after acknowledgement or
// authoritative correction. Peer revisions and timestamps are not local order.
func TestCronNoticeJobSuppressionRelease(t *testing.T) {
	t.Run("acknowledgement before flush", func(t *testing.T) {
		m := cronNoticeFixture(t)
		for _, id := range []string{"a", "b"} {
			m, _ = m.cronNoticeEvent(cronNoticeEvent(id, "job", snapshotFixedNow))
		}
		m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: []string{"a", "b"}}
		m, cmd := m.cronNoticeFlush(cronNoticeTickFor(m))
		require.Nil(t, cmd)
		require.Nil(t, m.toast)
	})
	t.Run("workflow-only suppression", func(t *testing.T) {
		// Workflow-only observations do not share an empty-job suppression bucket.
		m := cronNoticeFixture(t)
		m, _ = m.cronNoticeEvent(cronNoticeEvent("first", "", snapshotFixedNow))
		m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
		m, cmd := m.cronNoticeEvent(cronNoticeEvent("first", "", snapshotFixedNow))
		require.Nil(t, cmd, "same-UID workflow replay must stay suppressed without a job bucket")
		require.Empty(t, m.cronNotices.pending)
		m, cmd = m.cronNoticeEvent(cronNoticeEvent("second", "", snapshotFixedNow))
		require.NotNil(t, cmd)
	})
	tests := []struct {
		name string
		run  func(*testing.T, Model)
	}{
		{"snapshot at arrival", func(t *testing.T, m Model) {
			m.cron.data.lookup = map[string]generated.CronRun{"old": {UID: "old", Status: "succeeded", Revision: 50, UpdatedAt: snapshotFixedNow}}
			first := cronNoticeEvent("old", "job", snapshotFixedNow)
			first.cronRun.Revision = 1
			m, _ = updateModel(m, first) // the real handler marks the run changed until a later read
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
			m.toast = nil
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			require.NotNil(t, m.toast)
			require.Contains(t, m.cronNotices.announced, "c")
		}},
	}
	T := snapshotFixedNow
	for _, order := range []string{"read before flush", "flush before read", "winning failure"} {
		tests = append(tests, struct {
			name string
			run  func(*testing.T, Model)
		}{order, func(t *testing.T, m Model) {
			job := m.cron.data.jobs[0].UID
			loaded := generated.CronRun{UID: "old", JobUID: &job, Status: "succeeded", Revision: 5, CreatedAt: T, UpdatedAt: T.Add(time.Minute), EndedAt: new(T.Add(time.Minute))}
			m.cron.data.runs = append(m.cron.data.runs, loaded)
			event := cronNoticeEvent("old", job, T)
			event.cronRun.Revision = 9 // a source peer's revision, incomparable with local revision 5
			next, _ := m.handleEventReceived(event)
			m = next.(Model)
			require.Contains(t, m.cronNotices.pending, "old", "before any later read, the event is the only evidence")
			if order == "winning failure" {
				// The event won: the store's row became failed at local revision 6.
				won := loaded
				won.Status, won.Revision, won.UpdatedAt, won.EndedAt = "failed", 6, T, new(T)
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
				// A read issued before the correction can land afterward. Its success
				// cannot settle the newer change mark, even though it captured the failure.
				probe := land(m, m.cron.events-1, row(m, "succeeded", 7, T.Add(30*time.Second)))
				require.NotZero(t, probe.cron.changed["r"].at)
				if !flushed {
					probe, _ = probe.cronNoticeFlush(cronNoticeTickFor(probe))
					require.NotNil(t, probe.toast, "the older success must not cancel the failure window")
				}
				probe = deliver(probe, "before-read", "failed", T.Add(time.Minute))
				require.NotContains(t, probe.cronNotices.pending, "before-read", "the correction's read must settle before releasing the job")
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
	// A correction read inside the window settles the pending notice, even when
	// a later refresh in that window cannot read the run or sees a newer event.
	for _, mode := range []string{"later read failed", "later event unsettled"} {
		t.Run("settled correction survives the window/"+mode, func(t *testing.T) {
			m := cronNoticeFixture(t)
			m = land(m, m.cron.events, row(m, "running", 5, T))
			m = deliver(m, "r", "failed", T.Add(time.Minute))
			require.Contains(t, m.cronNotices.pending, "r")
			m = deliver(m, "r", "succeeded", T.Add(30*time.Second))
			m = land(m, m.cron.events, row(m, "succeeded", 7, T.Add(30*time.Second)))
			require.NotContains(t, m.cron.changed, "r", "the correction's read settled the run")
			switch mode {
			case "later read failed":
				m = deliver(m, "other", "running", T.Add(time.Minute))
				p := *m.cron.data
				p.events, p.generation = m.cron.events, m.cron.readGen
				p.runsErr = errors.New("daemon offline")
				p.runErr = map[string]error{"r": errors.New("daemon offline")}
				p.lastErr = map[string]error{m.cron.data.jobs[0].UID: errors.New("daemon offline")}
				m, _ = updateModel(m, cronLoadedMsg(p))
			case "later event unsettled":
				m = deliver(m, "r", "succeeded", T.Add(31*time.Second))
				require.NotZero(t, m.cron.changed["r"].at)
			}
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			require.Nil(t, m.toast, "the settled correction must cancel the pending notice")
			m, cmd := m.cronNoticeEvent(cronNoticeEvent("next", m.cron.data.jobs[0].UID, T.Add(2*time.Minute)))
			require.NotNil(t, cmd, "a later failure of the job must still be announced")
		})
	}
}

// Older-page evidence settles a notice only if its request follows the latest
// event, regardless of whether the page or the project-head refresh lands first.
func TestCronNoticeOlderPageSettlement(t *testing.T) {
	T := snapshotFixedNow
	for _, requested := range []string{"before event", "after event", "overtaken"} {
		for _, pageFirst := range []bool{true, false} {
			for _, announced := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/page before head=%v/announced=%v", requested, pageFirst, announced), func(t *testing.T) {
					m := cronNoticeFixture(t)
					job := m.cron.data.jobs[0].UID
					kept := generated.CronRun{UID: "r", JobUID: &job, Status: "succeeded", Revision: 5, CreatedAt: T.Add(-time.Hour), UpdatedAt: T.Add(time.Minute), EndedAt: new(T.Add(time.Minute))}
					if requested == "before event" {
						kept.Status, kept.UpdatedAt, kept.EndedAt = "running", T.Add(-time.Hour), nil
					}
					head := cronHistoryRows(m.cron.data.runs[0], 100)
					kept.CreatedAt = head[len(head)-1].CreatedAt.Add(-time.Minute)
					if requested == "before event" {
						kept.UpdatedAt = kept.CreatedAt
					}
					data := *m.cron.data
					data.runs = append(slices.Clone(head), kept)
					m.api = &cronReadTestAPI{data: data}
					m.cron.data.runs = head
					m.cron.runPage = cronHistoryPage{runs: head, next: head[len(head)-1].UID, ready: true}
					event := cronNoticeEvent("r", job, T.Add(time.Minute))
					if requested != "before event" {
						m, _ = updateModel(m, event)
					}
					m, cmd := m.cronRequestPage("", false)
					require.NotNil(t, cmd)
					page := cmd()
					if requested == "before event" {
						m, _ = updateModel(m, event)
					}
					require.NotContains(t, m.cron.changed, "r", "an unloaded run has no change mark")
					require.Contains(t, m.cronNotices.pending, "r")
					if announced {
						m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
						require.Contains(t, m.cronNotices.announced, "r")
						m.toast = nil
					}
					if requested == "overtaken" {
						m, _ = updateModel(m, cronNoticeEvent("r", job, T.Add(2*time.Minute)))
					}
					if pageFirst {
						m, _ = updateModel(m, page)
					}
					m = cronSettleTest(m)
					if !pageFirst {
						m, _ = updateModel(m, page)
					}
					got, loaded := m.cron.data.run("r")
					require.True(t, loaded, "the page remains available for display")
					require.Equal(t, kept.Status, got.Status)
					fresh := requested == "after event"
					if !announced {
						m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
						require.Equal(t, !fresh, m.toast != nil, "only a page read after the latest event settles the notice")
						require.Equal(t, !fresh, m.cronNotices.announced["r"].UID != "")
					} else {
						m = cronSettleTest(m)
						m, cmd = m.cronNoticeEvent(cronNoticeEvent("next", job, T.Add(3*time.Minute)))
						require.Equal(t, fresh, cmd != nil, "only settled success releases the job")
					}
				})
			}
		}
	}
}

// A read that settled an announced failure outlives the history that showed
// it: once a later head no longer carries the run, a corrected failure still
// releases its job and an uncorrected one still suppresses it.
func TestCronNoticeResolutionOutlivesHistory(t *testing.T) {
	for _, corrected := range []bool{true, false} {
		t.Run(fmt.Sprintf("corrected=%v", corrected), func(t *testing.T) {
			m := cronNoticeFixture(t)
			job := m.cron.data.jobs[0].UID
			m, _ = m.cronNoticeEvent(cronNoticeEvent("announced", job, snapshotFixedNow))
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			require.NotNil(t, m.toast)
			m.toast = nil
			newer := m.cron.data.runs[0]
			read := newer
			read.UID, read.Status, read.Revision, read.EndedAt = "announced", "failed", 2, new(snapshotFixedNow)
			if corrected {
				read.Status = "succeeded"
			}
			for _, head := range [][]generated.CronRun{{newer, read}, {newer}} {
				data := *m.cron.data
				data.runs, data.lookup, data.resetPages = head, nil, true
				data.lastRuns = map[string]generated.CronRun{job: newer}
				m = m.cronLoaded(cronLoadedMsg(data))
				if corrected && len(head) == 2 {
					require.Zero(t, m.cronUnseenCount())
					probe, cmd := m.cronNoticeEvent(cronNoticeEvent("new-failure", job, snapshotFixedNow))
					require.NotNil(t, cmd, "the loaded correction immediately releases its job")
					probe, _ = probe.cronNoticeFlush(cronNoticeTickFor(probe))
					require.NotNil(t, probe.toast)
				}

			}
			_, loaded := m.cron.data.run("announced")
			require.False(t, loaded, "the later head dropped the run from loaded history")
			m, cmd := m.cronNoticeEvent(cronNoticeEvent("next", job, snapshotFixedNow))
			require.Equal(t, corrected, cmd != nil, "the settled read, not the original event, decides the job")
		})
	}
}

// A last-run fill read after a correction settles that run, even when its
// exact UID read failed, so the corrected failure releases its job.
func TestCronNoticeLastRunFillSettlesCorrection(t *testing.T) {
	m := cronNoticeFixture(t)
	job := m.cron.data.jobs[0].UID
	old := m.cron.data.runs[0]
	old.UID, old.Status, old.Revision, old.EndedAt = "outside-head", "failed", 2, new(snapshotFixedNow)
	m.cron.data.runs = nil
	m.cron.data.lastRuns = map[string]generated.CronRun{job: old}
	m.cron.data.lookup = map[string]generated.CronRun{old.UID: old}
	m, _ = m.cronNoticeEvent(cronNoticeEvent(old.UID, job, snapshotFixedNow))
	m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
	require.NotNil(t, m.toast)
	m.toast = nil
	correction := cronNoticeEvent(old.UID, job, snapshotFixedNow)
	correction.cronRun.Status = "succeeded"
	m, _ = updateModel(m, correction)
	require.NotZero(t, m.cron.changed[old.UID].at)
	fixed := old
	fixed.Status, fixed.Revision = "succeeded", 3
	data := *m.cron.data
	data.events, data.generation = m.cron.events, m.cron.readGen
	data.runs = nil
	data.lastRuns, data.lastErr = map[string]generated.CronRun{job: fixed}, map[string]error{job: nil}
	data.runErr = map[string]error{old.UID: errors.New("context deadline exceeded")}
	m, _ = updateModel(m, cronLoadedMsg(data))
	require.NotContains(t, m.cron.changed, old.UID, "the fill read the run after its correction")
	m, cmd := m.cronNoticeEvent(cronNoticeEvent("next", job, snapshotFixedNow))
	require.NotNil(t, cmd, "the corrected failure releases its job")
}
