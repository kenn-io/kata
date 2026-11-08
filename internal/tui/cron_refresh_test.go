package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronRefreshTestAPI struct {
	*cronWaitFetchAPI
	mu    sync.Mutex
	reads []string
	exact func(context.Context, string) (generated.CronRun, error)
}

func (a *cronRefreshTestAPI) readCronRun(ctx context.Context, _ int64, uid string) (generated.CronRun, error) {
	a.mu.Lock()
	a.reads = append(a.reads, uid)
	a.mu.Unlock()
	return a.exact(ctx, uid)
}
func cronRefreshFixture(t *testing.T, n int) (Model, *cronRefreshTestAPI) {
	t.Helper()
	m := cronSeenFixture(t)
	m.cron.stack = nil
	m.cron.data = &cronData{}
	m.cron.runPage, m.cron.jobPage = cronHistoryPage{}, cronHistoryPage{}
	for i := range n {
		m.cron.runPage.runs = append(m.cron.runPage.runs, generated.CronRun{UID: fmt.Sprintf("o-%03d", i), Status: "succeeded", Revision: 2, CreatedAt: snapshotFixedNow.Add(-time.Duration(i) * time.Minute), UpdatedAt: snapshotFixedNow})
	}
	m = m.cronRemember(m.cron.runPage.runs)
	m.cron.runPage.ready = true
	a := &cronRefreshTestAPI{cronWaitFetchAPI: &cronWaitFetchAPI{cronReadTestAPI: &cronReadTestAPI{}, waiter: waitReadAPI(nil)}}
	a.exact = func(_ context.Context, uid string) (generated.CronRun, error) {
		i := slices.IndexFunc(m.cron.runPage.runs, func(r generated.CronRun) bool { return r.UID == uid })
		r := m.cron.runPage.runs[i]
		r.Status = "failed"
		r.Revision = 3
		return r, nil
	}
	m.api = a
	return m, a
}
func cronNameTest(m Model, uid string, revision int64) Model {
	msg := cronNoticeEvent(uid, "", snapshotFixedNow)
	msg.cronRun.Revision = revision
	m, _ = updateModel(m, msg)
	return m
}
func cronLoadTest(t *testing.T, m Model) Model {
	t.Helper()
	require.True(t, m.cron.out)
	m, _ = updateModel(m, m.fetchCron()())
	return m
}

// An event naming a retained run refreshes it even beyond the fallback cap.
func TestCronEventRefreshReadsLoadedRuns(t *testing.T) {
	t.Run("named retained run beyond fallback cap", func(t *testing.T) {
		m, a := cronRefreshFixture(t, 103)
		m = cronNameTest(m, "o-102", 1)
		m = cronLoadTest(t, m)
		require.Contains(t, a.reads, "o-102")
		r, ok := m.cron.data.run("o-102")
		require.True(t, ok)
		require.Equal(t, "failed", r.Status)
		require.True(t, m.cronRunUnseen(m.cronProjectUID(), r))
		require.Empty(t, m.cron.changed)
	})
	for _, status := range []string{"unknown", "running"} {
		t.Run(status, func(t *testing.T) {
			m, a := cronRefreshFixture(t, 1)
			r := m.cron.runPage.runs[0]
			r.Status = status
			r.IssueUID = new("wait-issue")
			m.cron.runPage.runs[0] = r
			m = m.cronRemember([]generated.CronRun{r})
			a.exact = func(context.Context, string) (generated.CronRun, error) {
				r.Status = "succeeded"
				r.Revision++
				return r, nil
			}
			m = cronNameTest(m, r.UID, 1)
			m = cronLoadTest(t, m)
			got, _ := m.cron.data.run(r.UID)
			require.Equal(t, "succeeded", got.Status)
			require.Zero(t, m.cronUnseenCount())
			require.Empty(t, m.cron.data.waits)
		})
	}
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("evicted previous head/failed=%v", failed), func(t *testing.T) {
			m, a := cronRefreshFixture(t, 1)
			m.cron.data.runs = slices.Clone(m.cron.runPage.runs)
			if failed {
				a.data.runsErr = errors.New("offline")
			}
			m = cronNameTest(m, "o-000", 1)
			m = cronLoadTest(t, m)
			require.Contains(t, a.reads, "o-000")
			r, _ := m.cron.data.run("o-000")
			require.Equal(t, "failed", r.Status)
		})
	}
}

func TestCronNamedBurstDrains(t *testing.T) {
	for _, failures := range []int{0, 10, 100} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			m, a := cronRefreshFixture(t, 130)
			original := a.exact
			a.exact = func(ctx context.Context, uid string) (generated.CronRun, error) {
				var i int
				_, err := fmt.Sscanf(uid, "o-%03d", &i)
				if err != nil {
					return generated.CronRun{}, err
				}
				if i >= 130-failures {
					return generated.CronRun{}, errors.New("offline")
				}
				return original(ctx, uid)
			}
			m.view = viewList
			for i := 129; i >= 0; i-- {
				m = cronNameTest(m, fmt.Sprintf("o-%03d", i), 1)
			}
			m, _ = updateModel(m, refetchTickMsg{})
			m.view = viewCron
			m, _ = updateModel(m, refetchTickMsg{})
			m = cronLoadTest(t, m)
			require.Len(t, a.reads, 100)
			for i := 30; i < 130; i++ {
				require.Contains(t, a.reads, fmt.Sprintf("o-%03d", i))
			}
			if failures == 100 {
				require.Len(t, m.cron.changed, 130)
				require.False(t, m.cron.out)
				for range 3 {
					m, _ = updateModel(m, refetchTickMsg{})
					require.False(t, m.cron.out)
				}
				m, _ = updateModel(m, eventReceivedMsg{eventType: "cron.job.updated", projectID: 7})
				data := cronData(m.fetchCron()().(cronLoadedMsg))
				for i := range 30 {
					require.Equal(t, fmt.Sprintf("o-%03d", 29-i), data.changed[i], "fetchCron preserves deferred event order")
				}
				return
			}
			require.True(t, m.cron.out, "deferred events must drain without another event")
			a.exact = original
			a.reads = nil
			data := cronData(m.fetchCron()().(cronLoadedMsg))
			for i := range 30 {
				require.Equal(t, fmt.Sprintf("o-%03d", 29-i), data.changed[i], "fetchCron preserves deferred event order")
			}
			if failures > 0 {
				require.Len(t, data.changed, 40)
			}
			m, _ = updateModel(m, cronLoadedMsg(data))
			require.Len(t, a.reads, 100)
			require.Empty(t, m.cron.changed)
			require.False(t, m.cron.out)
			require.Equal(t, 130, m.cronUnseenCount())
		})
	}
}

func TestCronExactResultAdmitsAttention(t *testing.T) {
	for _, mode := range []string{"becomes running", "stale running", "offline", "wrong UID"} {
		t.Run(mode, func(t *testing.T) {
			m, a := cronRefreshFixture(t, 1)
			r := m.cron.runPage.runs[0]
			r.Status = "running"
			r.Actor = "operator"
			r.IssueUID = new("wait-issue")
			if mode == "becomes running" {
				r.Status = "failed"
			}
			p := cronData{inspectedRuns: []generated.CronRun{r}, historyRuns: []generated.CronRun{r}}
			if mode == "stale running" {
				for i := range 20 {
					old := r
					old.UID = fmt.Sprint(i)
					old.IssueUID = new(old.UID)
					p.inspectedRuns = append(p.inspectedRuns, old)
					p.historyRuns = append(p.historyRuns, old)
				}
			}
			a.exact = func(_ context.Context, uid string) (generated.CronRun, error) {
				fresh := r
				fresh.Status = "running"
				switch mode {
				case "offline":
					return generated.CronRun{}, errors.New("offline")
				case "wrong UID":
					fresh.UID = "foreign"
				}
				if uid != r.UID {
					fresh.UID = uid
					fresh.Status = "succeeded"
				}
				return fresh, nil
			}
			cronEnrich(t.Context(), a, 7, &p)
			require.Equal(t, map[string]int{"wait-issue": 1}, a.waiter.waitCalls)
			require.Equal(t, "blocked", cronRunHealth(p, p.lookup[r.UID]).state)
			require.NotContains(t, p.lookup, "foreign")
			if mode == "offline" || mode == "wrong UID" {
				require.Error(t, p.runErr[r.UID])
				require.Equal(t, r, p.lookup[r.UID])
			}
		})
	}
}

func TestCronResetDropsUnreadHistory(t *testing.T) {
	for _, mode := range []string{"inactive", "open", "failed head"} {
		t.Run(mode, func(t *testing.T) {
			m, a := cronRefreshFixture(t, 1)
			r := m.cron.runPage.runs[0]
			r.Status = "running"
			r.IssueUID = new("old-issue")
			m.cron.runPage.runs = nil
			m.cron.jobPage = cronHistoryPage{job: "job", runs: []generated.CronRun{r}, ready: true}
			m.cron.data.lookup = map[string]generated.CronRun{r.UID: r}
			m.view = viewList
			m = cronNameTest(m, r.UID, 1)
			if mode == "open" {
				m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, r.UID}}}
			}
			if mode == "failed head" {
				a.data.runsErr = errors.New("offline")
			}
			m.view = viewCron
			m, _ = updateModel(m, tea.KeyPressMsg{Code: 'r', Text: "r"})
			m = cronLoadTest(t, m)
			if mode == "failed head" {
				require.NotEmpty(t, m.cron.jobPage.runs)
				require.True(t, m.cron.resetPages)
				return
			}
			require.Empty(t, m.cron.jobPage.runs)
			require.Empty(t, m.cron.data.waits)
			if mode == "open" {
				require.Contains(t, a.reads, r.UID)
				got, _ := m.cron.data.run(r.UID)
				require.Equal(t, "failed", got.Status)
			} else {
				require.Empty(t, a.reads)
				require.Empty(t, m.cronLoadedRuns())
			}
		})
	}
}

func TestCronSharedParentEvidence(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(parentFirst), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				native := cronFixture(t).cron.data.jobs[0]
				nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&native.Definition)
				adapter := native
				adapter.UID = "adapter"
				adapter.Definition.Action = generated.ActionResponse{Kind: "shell"}
				p := cronData{jobs: []generated.CronJob{native, adapter}, runs: []generated.CronRun{{UID: "native-run", JobUID: new(native.UID), Status: "succeeded"}}}
				for i := range 20 {
					p.runs = append(p.runs, generated.CronRun{UID: fmt.Sprint(i), Status: "running", IssueUID: new(fmt.Sprint(i)), Actor: "operator"})
				}
				parentDone, fillDone := make(chan struct{}), make(chan struct{})
				a := waitReadAPI(func(context.Context, string) ([]generated.CronRun, error) {
					if parentFirst {
						select {
						case <-parentDone:
						case <-time.After(time.Second):
							return nil, errors.New("parent read did not finish")
						}
					}
					close(fillDone)
					return []generated.CronRun{{UID: "fill", JobUID: new(adapter.UID), Status: "running", Actor: "operator", IssueUID: new(nextParentUID)}}, nil
				})
				a.wait = func(_ context.Context, uid string) (cronParent, error) {
					if uid == nextParentUID {
						if !parentFirst {
							select {
							case <-fillDone:
							case <-time.After(time.Second):
								return cronParent{}, errors.New("fill read did not finish")
							}
						}
						close(parentDone)
					}
					return cronParent{open: true, metadata: map[string]any{"work.attention": "needs-human"}}, nil
				}
				cronEnrich(t.Context(), a, 7, &p)
				require.Len(t, a.waitCalls, 21)
				require.Equal(t, 1, a.waitCalls[nextParentUID])
				require.NoError(t, p.waitErr[nextParentUID])
				require.Equal(t, "blocked", cronRunHealth(p, p.lastRuns[adapter.UID]).state)
			})
		})
	}
}

func TestCronNamedRunDuringFetch(t *testing.T) {
	m, a := cronRefreshFixture(t, 103)
	m = cronNameTest(m, "o-102", 1)
	first := m.fetchCron()
	seq := m.cron.changed["o-102"].seq
	m = cronNameTest(m, "o-102", 2)
	m, _ = updateModel(m, first())
	require.Equal(t, seq, m.cron.changed["o-102"].seq)
	require.Greater(t, m.cron.changed["o-102"].at, m.cron.data.events)
	m, _ = updateModel(m, refetchTickMsg{})
	require.True(t, m.cron.out)
	a.reads = nil
	m = cronLoadTest(t, m)
	require.Contains(t, a.reads, "o-102", "event received during first fetch must survive it")
	require.False(t, m.cron.out)

	t.Run("unloaded event", func(t *testing.T) {
		m, _ := cronRefreshFixture(t, 1)
		m = cronNameTest(m, "unloaded-x", 9)
		require.Empty(t, m.cron.changed)
	})
}

func TestCronLosingHigherSourceRevision(t *testing.T) {
	m, a := cronRefreshFixture(t, 202)
	original := a.exact
	a.exact = func(_ context.Context, uid string) (generated.CronRun, error) {
		for _, r := range m.cron.runPage.runs {
			if r.UID == uid {
				return r, nil
			}
		}
		return generated.CronRun{}, errors.New("missing")
	}
	m.view = viewList
	for i := range 100 {
		m = cronNameTest(m, fmt.Sprintf("o-%03d", i), 9)
	}
	m, _ = updateModel(m, refetchTickMsg{})
	m.view = viewCron
	m, _ = updateModel(m, refetchTickMsg{})
	m = cronLoadTest(t, m)
	require.Empty(t, m.cron.changed)
	require.False(t, m.cron.out)
	a.exact = original
	a.reads = nil
	m = cronNameTest(m, "o-201", 1)
	m = cronLoadTest(t, m)
	require.Contains(t, a.reads, "o-201")
	r, _ := m.cron.data.run("o-201")
	require.Equal(t, "failed", r.Status)
}

func TestCronRefreshSubjects(t *testing.T) {
	p := cronData{}
	head := map[string]generated.CronRun{}
	for i := range 100 {
		r := generated.CronRun{UID: fmt.Sprintf("h-%03d", i), Status: "succeeded"}
		p.historyRuns = append(p.historyRuns, r)
		if i != 99 {
			head[r.UID] = r
		}
	}
	for i := range 150 {
		p.historyRuns = append(p.historyRuns, generated.CronRun{UID: fmt.Sprintf("o-%03d", i), Status: "succeeded"})
	}
	p.inspectedRuns = []generated.CronRun{p.historyRuns[240]}
	for i := 245; i <= 247; i++ {
		p.historyRuns[i].Status = "running"
	}
	p.changed = []string{"o-149", "o-120", "h-050", "unloaded-x"}
	got, deferred := cronRefreshSubjects(&p, head, true)
	ids := func(rows []generated.CronRun) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.UID)
		}
		return out
	}
	want := []string{"o-140", "o-149", "o-120", "o-145", "o-146", "o-147", "h-099"}
	for i := range 94 {
		want = append(want, fmt.Sprintf("o-%03d", i))
	}
	require.Equal(t, want, ids(got))
	require.False(t, deferred)
	got, deferred = cronRefreshSubjects(&p, head, false)
	require.Equal(t, []string{"o-140"}, ids(got))
	require.False(t, deferred)
	p.changed = nil
	for i := range 101 {
		p.changed = append(p.changed, fmt.Sprintf("o-%03d", i))
	}
	_, deferred = cronRefreshSubjects(&p, head, true)
	require.True(t, deferred)
}

func TestCronRefreshQueueAttentionBound(t *testing.T) {
	for _, mode := range []string{"fill", "exact"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a := cronRefreshFixture(t, 16)
				p := cronData{inspectedRuns: m.cron.runPage.runs}
				entered := make(chan struct{}, 16)
				release := make(chan struct{}, 16)
				parentCalled := make(chan int, 1)
				a.waiter.wait = func(context.Context, string) (cronParent, error) {
					a.mu.Lock()
					defer a.mu.Unlock()
					parentCalled <- len(a.reads) // exact reads started before attention
					return cronParent{open: true}, nil
				}
				a.exact = func(ctx context.Context, uid string) (generated.CronRun, error) {
					if mode == "exact" && uid == "o-000" {
						return generated.CronRun{UID: uid, Status: "running", IssueUID: new("late")}, nil
					}
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return generated.CronRun{}, ctx.Err()
					}
					return generated.CronRun{UID: uid, Status: "succeeded"}, nil
				}
				if mode == "fill" {
					j := cronFixture(t).cron.data.jobs[0]
					p.jobs = []generated.CronJob{j}
					// Override the list read only for this job fill, while retaining the head API.
					a.cronWaitFetchAPI = &cronWaitFetchAPI{cronReadTestAPI: &cronReadTestAPI{data: cronData{runs: []generated.CronRun{{UID: "fill", JobUID: new(j.UID), Status: "running", IssueUID: new("late")}}}}, waiter: a.waiter}
				}
				done := make(chan struct{})
				go func() { cronEnrich(t.Context(), a, 7, &p); close(done) }()
				// A dynamically admitted attention read must run before a ninth exact
				// read can overtake it (four queued plus four in flight). Releasing one
				// read only once every worker blocks makes reads start in queue order,
				// not goroutine wake-up order.
				for {
					synctest.Wait()
					select {
					case ahead := <-parentCalled:
						if mode == "exact" {
							ahead-- // o-000 itself revealed the attention
						}
						require.LessOrEqual(t, ahead, 8, "more than eight exact reads overtook attention")
					case <-entered:
						release <- struct{}{}
						continue
					case <-done:
						t.Fatal("refresh ended without attention")
					default:
						t.Fatal("attention read did not start")
					}
					break
				}
				for range 16 {
					release <- struct{}{}
				}
				<-done
				require.Len(t, a.reads, 16)
				require.Len(t, p.lookup, 16)
			})
		})
	}
}

func TestCronSuccessfulHeadClearsMarkWithoutExactRead(t *testing.T) {
	m, a := cronRefreshFixture(t, 1)
	r := m.cron.runPage.runs[0]
	r.Status = "failed"
	r.Revision++
	a.data.runs = []generated.CronRun{r}
	m = cronNameTest(m, r.UID, 9)
	m = cronLoadTest(t, m)
	require.Empty(t, a.reads)
	require.Empty(t, m.cron.changed)
	require.Equal(t, 1, m.cronUnseenCount())
}

// Matching counters name no run, so opening a retained row after missed events
// or skipped or failed reads must itself read the run by UID.
func TestCronOpeningRetainedRunRefreshesByUID(t *testing.T) {
	cases := []struct {
		mode     string
		closeKey tea.KeyPressMsg
	}{{mode: "failed"}, {mode: "running"}, {mode: "closed before load"}, {mode: "behind narrow"}}
	for _, key := range []tea.KeyPressMsg{keyRune('?'), {Code: tea.KeyEscape}, keyRune('h')} {
		cases = append(cases, struct {
			mode     string
			closeKey tea.KeyPressMsg
		}{"behind help", key})
	}
	for _, tc := range cases {
		mode := tc.mode
		name := mode
		if mode == "behind help" {
			name += "/" + tc.closeKey.String()
		}
		t.Run(name, func(t *testing.T) {
			m, a := cronRefreshFixture(t, 1)
			r := m.cron.runPage.runs[0]
			r.Status, r.Actor, r.IssueUID = "running", "operator", new("wait-issue")
			m.cron.runPage.runs[0] = r
			m = m.cronRemember([]generated.CronRun{r})
			if mode == "running" {
				a.exact = func(context.Context, string) (generated.CronRun, error) { r.Revision = 3; return r, nil }
			}
			m.cron.tab = cronRuns
			_, _, lines := cronScreen(m)
			i := slices.IndexFunc(lines, func(l cronLine) bool { return l.target == cronRef{cronRuns, r.UID} })
			require.NotEqual(t, -1, i, "event retains the older row")
			m.cron.lists[cronRuns].cursor = i
			m = sendKey(m, tea.KeyEnter)

			if mode == "closed before load" {
				m = sendKey(m, tea.KeyEscape)
			}
			if mode == "behind help" {
				m = sendRune(m, '?')
				require.True(t, m.cron.help)
			}
			if mode == "behind narrow" {
				m, _ = updateModel(m, tea.WindowSizeMsg{Width: 60, Height: 24})
				require.Contains(t, stripANSI(m.View().Content), "needs more space")
			}
			m = cronLoadTest(t, m)
			got, _ := m.cron.data.run(r.UID)
			switch mode {
			case "failed":
				require.Equal(t, []string{r.UID}, a.reads)
				require.Equal(t, "failed", got.Status)
				require.Zero(t, m.cronUnseenCount(), "a failure shown in the open run is seen")
			case "running":
				require.Equal(t, map[string]int{"wait-issue": 1}, a.waiter.waitCalls)
				require.Contains(t, cronScreenText(m), "blocked")
			case "closed before load":
				require.Equal(t, "failed", got.Status)
				require.Equal(t, 1, m.cronUnseenCount(), "a failure never shown stays unseen")
			case "behind help":
				require.Equal(t, "failed", got.Status)
				require.Equal(t, 1, m.cronUnseenCount(), "a failure refreshed behind Help is not yet shown")
				require.False(t, m.cronSeen.session[m.cronProjectUID()][r.UID])
				m, _ = updateModel(m, tc.closeKey)
				require.False(t, m.cron.help)
				require.Equal(t, cronRef{cronRuns, r.UID}, m.cron.stack[len(m.cron.stack)-1].ref)
				require.Zero(t, m.cronUnseenCount(), "closing Help shows the failed run")
			case "behind narrow":
				require.Equal(t, "failed", got.Status)
				require.Equal(t, 1, m.cronUnseenCount(), "a failure refreshed behind the narrow hint is not yet shown")
				require.False(t, m.cronSeen.session[m.cronProjectUID()][r.UID])
				m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 24})
				require.Contains(t, stripANSI(m.View().Content), "failed")
				require.Zero(t, m.cronUnseenCount(), "widening the terminal shows the failed run")
			}
		})
	}
}
