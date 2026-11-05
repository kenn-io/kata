package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

// Contract: the bounded cache merge is a deterministic capped union and max,
// preserving all projects. Only explicit capacity eviction can lose membership.
func TestCronSeenMergeAlgebra(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		draw := func() cronSeenStore {
			ids := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint64](0, ^uint64(0))).MaxSize(40))
			runs := make([]string, len(ids))
			for i, id := range ids {
				runs[i] = fmt.Sprintf("%016x", id)
			}
			stamp := snapshotFixedNow.Add(time.Duration(hegel.Draw(ht, hegel.Integers[uint32](0, ^uint32(0)))))
			return cronSeenStore{Version: 1, Projects: map[string]cronSeenProject{"project": {ClearedThrough: stamp, Runs: runs}}}
		}
		a, b, c := draw(), draw(), draw()
		ab := cronSeenMerge(a, b)
		if !reflect.DeepEqual(ab, cronSeenMerge(b, a)) {
			ht.Fatal("merge is not commutative")
		}
		if !reflect.DeepEqual(cronSeenMerge(ab, c), cronSeenMerge(a, cronSeenMerge(b, c))) {
			ht.Fatal("merge is not associative")
		}
		canonical := cronSeenMerge(a, cronSeenStore{})
		if !reflect.DeepEqual(canonical, cronSeenMerge(canonical, canonical)) {
			ht.Fatal("merge is not idempotent")
		}
		latest := a.Projects["project"].ClearedThrough
		if other := b.Projects["project"].ClearedThrough; other.After(latest) {
			latest = other
		}
		if !ab.Projects["project"].ClearedThrough.Equal(latest) {
			ht.Fatal("merge must retain the maximum clear watermark")
		}
		for _, id := range canonical.Projects["project"].Runs {
			if !slices.Contains(ab.Projects["project"].Runs, id) {
				ht.Fatalf("merge lost %q", id)
			}
		}
	}, hegel.WithTestCases(300))

	t.Run("capacity", func(t *testing.T) {
		ids := make([]string, 1100)
		for i := range ids {
			ids[i] = fmt.Sprintf("%04d", i)
		}
		s := cronSeenMerge(cronSeenStore{Projects: map[string]cronSeenProject{"p": {Runs: ids}}}, cronSeenStore{})
		require.Len(t, s.Projects["p"].Runs, 1000)
		require.NotContains(t, s.Projects["p"].Runs, "0000")
		require.Contains(t, s.Projects["p"].Runs, "1099")
	})
}

func TestCronSeenDecodeRejectsInvalid(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"version":2}`, `{"version":1,"projects":{"p":{"cleared_through":"bad"}}}`} {
		_, err := cronSeenDecode([]byte(raw))
		require.Error(t, err)
	}
}

func TestCronSeenUnseen(t *testing.T) {
	for _, status := range []string{"running", "succeeded", "cancelled", "failed", "unknown"} {
		t.Run(status, func(t *testing.T) {
			at := snapshotFixedNow.Add(123 * time.Nanosecond)
			r := generated.CronRun{UID: "run", Status: status, UpdatedAt: at}
			before := cronSeenProject{ClearedThrough: snapshotFixedNow}
			require.Equal(t, status == "failed" || status == "unknown", before.unseen(r))
			before.Runs = []string{"run"}
			require.False(t, before.unseen(r), "explicit acknowledgement wins")
			before.Runs = nil
			before.ClearedThrough = at
			require.False(t, before.unseen(r), "watermark equality acknowledges the run")
		})
	}
}

func TestCronSeenFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui", "cron-seen.json")
	empty, err := cronSeenLoad(t.Context(), path)
	require.NoError(t, err)
	require.Empty(t, empty.Projects)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = cronSeenLoad(ctx, path)
	require.ErrorIs(t, err, context.Canceled)
	// Sequential saves each supply only their own acknowledgement. Saving must
	// merge with disk rather than replace prior acknowledgements from either project.
	for i := range 20 {
		_, err := cronSeenSave(t.Context(), path, cronSeenStore{Projects: map[string]cronSeenProject{fmt.Sprint(i % 2): {Runs: []string{fmt.Sprint(i)}}}})
		require.NoError(t, err)
	}
	got, err := cronSeenLoad(t.Context(), path)
	require.NoError(t, err)
	require.Len(t, got.Projects["0"].Runs, 10)
	require.Len(t, got.Projects["1"].Runs, 10)
	require.True(t, slices.IsSorted(got.Projects["0"].Runs))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		info, err = os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	}
	require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0600))
	_, err = cronSeenSave(t.Context(), path, empty)
	require.Error(t, err)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir()
	require.NoError(t, err)
	require.Equal(t, "corrupt", string(raw))
}

func cronSeenFixture(t *testing.T) Model {
	t.Helper()
	m := cronFixture(t)
	m.projectUIDByID = map[int64]string{7: "project-example"}
	m.cronSeen = cronSeenState{ready: true, data: cronSeenMerge(cronSeenStore{}, cronSeenStore{})}
	return m
}
func TestCronSeenLoadedCountAndKeys(t *testing.T) {
	fixture := func(t *testing.T) (Model, generated.CronRun) {
		t.Helper()
		m := cronSeenFixture(t)
		r := m.cron.data.runs[0]
		r.Status = "failed"
		r.UID = "failed"
		r.EndedAt = new(snapshotFixedNow.Add(17 * time.Nanosecond))
		unknown := r
		unknown.UID = "unknown"
		unknown.Status = "unknown"
		unknown.EndedAt = nil
		unknown.UpdatedAt = snapshotFixedNow.Add(23 * time.Nanosecond)
		success := r
		success.UID = "success"
		success.Status = "succeeded"
		m.cron.data.runs = []generated.CronRun{r, success}
		m.cron.data.lastRuns = map[string]generated.CronRun{*r.JobUID: r}
		m.cron.jobPage = cronHistoryPage{job: *r.JobUID, runs: []generated.CronRun{unknown, r}, ready: true}
		m.cron.runPage.runs = []generated.CronRun{r, unknown}
		m.cron.runPage.ready = true
		return m, unknown
	}
	t.Run("loaded count and banners", func(t *testing.T) {
		m, _ := fixture(t)
		require.Equal(t, 2, m.cronUnseenCount())
		m.width = 160
		for _, tab := range []int{cronJobs, cronRuns, cronWorkflows} {
			m.cron.tab = tab
			require.Contains(t, stripANSI(renderCron(m)), "2 unseen failed")
		}
	})
	t.Run("review and clear keys", func(t *testing.T) {
		m, unknown := fixture(t)
		m = sendRune(m, 'u')
		require.Equal(t, cronRuns, m.cron.tab)
		require.True(t, m.cron.unseenOnly)
		title, _, lines := cronScreen(m)
		require.Contains(t, title, "unseen only")
		require.Len(t, lines, 2)
		failed := slices.IndexFunc(lines, func(l cronLine) bool { return l.target.uid == "failed" })
		require.NotEqual(t, -1, failed)
		require.Contains(t, stripANSI(lines[failed].text), "• failed")
		m = m.cronFilter("no match")
		_, _, lines = cronScreen(m)
		require.Empty(t, lines, "unseen-only ANDs with the existing query")
		m = m.cronFilter("")
		m, cmd := m.routeCronKey(keyRune('l'))
		require.Nil(t, cmd)
		require.Equal(t, 1, m.cronUnseenCount(), "opening any followable run marks it")
		m = sendRune(m, 'U')
		require.Zero(t, m.cronUnseenCount())
		require.Equal(t, unknown.UpdatedAt, m.cronSeen.data.Projects["project-example"].ClearedThrough)
	})
	for _, open := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, keyRune('l')} {
		t.Run("narrow open waits for resize/"+open.String(), func(t *testing.T) {
			m, _ := fixture(t)
			m = resizeModel(sendRune(m, 'u'), 60, 24)
			m, _ = updateModel(m, open)
			require.Equal(t, cronRuns, m.cron.stack[len(m.cron.stack)-1].ref.kind)
			require.Contains(t, stripANSI(m.View().Content), "needs more space")
			require.Equal(t, 2, m.cronUnseenCount(), "a run opened behind the narrow hint is not yet shown")
			m = resizeModel(m, 100, 24)
			require.NotContains(t, stripANSI(m.View().Content), "needs more space")
			require.Equal(t, 1, m.cronUnseenCount(), "widening shows the opened run")
		})
	}
	t.Run("roundtrip and project identity", func(t *testing.T) {
		m, _ := fixture(t)
		m = m.cronClearSeen()
		wire, err := json.Marshal(m.cronSeen.data)
		require.NoError(t, err)
		m.cronSeen.data, err = cronSeenDecode(wire)
		require.NoError(t, err)
		require.Zero(t, m.cronUnseenCount())
		// A different project on the same local numeric id has separate state.
		m.projectUIDByID[7] = "other-project"
		require.Equal(t, 2, m.cronUnseenCount())
	})
	t.Run("golden", func(t *testing.T) {
		defer snapshotInit(t)()
		m := cronSeenFixture(t)
		m.width = 120
		r := m.cron.data.runs[0]
		r.Status = "failed"
		m.cron.data.runs = []generated.CronRun{r}
		m.cron.data.lastRuns = map[string]generated.CronRun{*r.JobUID: r}
		m.cron.runPage.runs = []generated.CronRun{r}
		m = sendRune(m, 'u')
		assertGolden(t, "cron-runs-unseen", m.viewBody())
	})

	t.Run("filter preserves run order", func(t *testing.T) {
		m := cronSeenFixture(t)
		old := m.cron.data.runs[0]
		old.UID, old.Status = "01ARZ3NDEKTSV4RRFFQ69G5FAA", "failed"
		old.CreatedAt = snapshotFixedNow.Add(-time.Hour)
		old.EndedAt = new(old.CreatedAt)
		newest := old
		newest.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAB"
		newest.CreatedAt = snapshotFixedNow
		newest.EndedAt = new(newest.CreatedAt)
		rows := []generated.CronRun{newest, old}
		m.cron.tab = cronRuns
		m.cron.data.runs = rows
		m.cron.data.lastRuns = nil
		m.cron.data.lookup = nil
		m.cron.runPage = cronHistoryPage{runs: rows, ready: true}
		_, _, before := cronScreen(m)
		m.cron.unseenOnly = true
		_, _, after := cronScreen(m)
		require.Equal(t, before[0].target, after[0].target, "enabling a filter must retain the newest-first run order")
	})
}

func TestCronSeenOpeningOldRunAtCapacityIsSeenForSession(t *testing.T) {
	m := cronSeenFixture(t)
	r := m.cron.data.runs[0]
	r.UID = "0000"
	r.Status = "failed"
	m.cron.data.runs = []generated.CronRun{r}
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%04d", i+1)
	}
	m.cronSeen.data.Projects["project-example"] = cronSeenProject{Runs: ids}
	m = m.cronMarkSeen(r.UID)
	require.Zero(t, m.cronUnseenCount(), "an immediately evicted old mark remains seen until restart")
	require.NotContains(t, m.cronSeen.data.Projects["project-example"].Runs, r.UID)
}

func TestCronSeenUpdateSchedulesPersistence(t *testing.T) {
	m := cronSeenFixture(t)
	r := m.cron.data.runs[0]
	r.Status = "failed"
	m.cron.data.runs = []generated.CronRun{r}
	m.cronSeen.path = filepath.Join(t.TempDir(), "seen.json")
	next, cmd := updateModel(m, keyRune('U'))
	require.NotNil(t, cmd)
	wrote := false
	for _, msg := range runCmdTree(cmd) {
		if result, ok := msg.(cronSeenResultMsg); ok {
			require.True(t, result.write)
			require.NoError(t, result.err)
			wrote = true
		}
	}
	require.True(t, wrote, "Update must return the actual persistence command")
	require.True(t, next.cronSeen.writing, "the Update loop must dispatch the durable write after the clear action")
}

// Quit does not wait for an outstanding seen write, and an acknowledgement
// made during it is never dispatched. The final model persists once more,
// through the same locked merge, within a short bound.
func TestCronSeenQuitPersistsFinalAcknowledgements(t *testing.T) {
	fixture := func(t *testing.T, second bool) (Model, string) {
		t.Helper()
		m := cronSeenFixture(t)
		first := m.cron.data.runs[0]
		first.UID, first.Status = "first-failure", "failed"
		later := first
		later.UID = "second-failure"
		m.cron.data.runs = []generated.CronRun{first, later}
		path := filepath.Join(t.TempDir(), "cron-seen.json")
		m.cronSeen.path = path
		m, cmd := m.cronMarkSeen(first.UID).cronSeenSync()
		require.NotNil(t, cmd)
		require.True(t, m.cronSeen.writing, "the first write is outstanding and never finishes")
		if second {
			m, cmd = m.cronMarkSeen(later.UID).cronSeenSync()
			require.Nil(t, cmd, "single flight holds the newer acknowledgement")
			require.True(t, m.cronSeen.dirty)
		}
		return m, path
	}
	saved := func(t *testing.T, path string) []string {
		t.Helper()
		data, err := cronSeenLoad(t.Context(), path)
		require.NoError(t, err)
		return data.Projects["project-example"].Runs
	}
	t.Run("newer acknowledgement behind an outstanding write", func(t *testing.T) {
		m, path := fixture(t, true)
		var stderr strings.Builder
		cronSeenAfterQuit(t.Context(), m, &stderr)
		require.Equal(t, []string{"first-failure", "second-failure"}, saved(t, path))
		require.Empty(t, stderr.String())
	})
	t.Run("latest snapshot dispatched but unfinished", func(t *testing.T) {
		m, path := fixture(t, false)
		require.False(t, m.cronSeen.dirty)
		cronSeenAfterQuit(t.Context(), m, io.Discard)
		require.Equal(t, []string{"first-failure"}, saved(t, path))
	})
	t.Run("cancelled caller", func(t *testing.T) {
		m, path := fixture(t, true)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cronSeenAfterQuit(ctx, m, io.Discard)
		require.Equal(t, []string{"first-failure", "second-failure"}, saved(t, path))
	})
	t.Run("held lock bounds quit and reports", func(t *testing.T) {
		m, path := fixture(t, true)
		lock := flock.New(path + ".lock")
		require.NoError(t, lock.Lock())
		defer func() { _ = lock.Unlock() }()
		var stderr strings.Builder
		started := time.Now()
		cronSeenAfterQuit(t.Context(), m, &stderr)
		require.Less(t, time.Since(started), defaultHTTPTimeout, "quit waits less than an ordinary write")
		require.Contains(t, stderr.String(), "cron seen state not saved")
		require.NoFileExists(t, path)
	})
	t.Run("nothing to save", func(t *testing.T) {
		clean := cronSeenFixture(t)
		failed, _ := fixture(t, true)
		failed.cronSeen.err = errors.New("unreadable")
		loading, _ := fixture(t, true)
		loading.cronSeen.ready = false
		for _, m := range []Model{clean, failed, loading} {
			path := filepath.Join(t.TempDir(), "cron-seen.json")
			m.cronSeen.path = path
			var stderr strings.Builder
			cronSeenAfterQuit(t.Context(), m, &stderr)
			require.NoFileExists(t, path)
			require.Empty(t, stderr.String())
		}
	})
}

// The catalog can arrive after the run view; acknowledgement waits for its
// canonical project identity and must never follow a later scope switch.
func TestCronSeenDeferredProjectIdentity(t *testing.T) {
	for _, clear := range []bool{false, true} {
		for _, switched := range []bool{false, true} {
			t.Run(fmt.Sprintf("clear=%v/switched=%v", clear, switched), func(t *testing.T) {
				m := cronSeenFixture(t)
				r := m.cron.data.runs[0]
				r.Status = "failed"
				m.cron.data.runs = []generated.CronRun{r}
				m.projectUIDByID = nil
				if clear {
					m = m.cronClearSeen()
				} else {
					m = m.cronMarkSeen(r.UID)
				}
				require.Empty(t, m.cronSeen.data.Projects)
				if switched {
					m.scopeGen++
				}
				m.projectUIDByID = map[int64]string{7: "project-example"}
				m, _ = m.cronSeenSync()
				if switched {
					require.Empty(t, m.cronSeen.data.Projects)
				} else {
					require.Zero(t, m.cronUnseenCount(), "the deferred acknowledgement applies once identity resolves")
				}
			})
		}
	}
}

func TestCronSeenLoadsOnlyAtInitialization(t *testing.T) {
	// Models built without Init must not inject filesystem commands into an
	// unrelated update. The real TUI schedules its single startup read in Init.
	_, cmd := (Model{}).cronSeenSync()
	require.Nil(t, cmd)
	m := initialModel(Options{})
	m.cronSeen.path = filepath.Join(t.TempDir(), "seen.json")
	m, cmd = m.cronSeenSync()
	require.Nil(t, cmd)
	priorMode, priorDark := activeColorMode, activeHasDarkBackground
	t.Setenv("KATA_COLOR_MODE", "none")
	applyDefaultColorMode()
	defer applyColorMode(priorMode, priorDark)
	ctx, cancel := context.WithTimeout(t.Context(), defaultHTTPTimeout)
	defer cancel()
	var loaded *cronSeenResultMsg
	program := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		if result, ok := msg.(cronSeenResultMsg); ok {
			loaded = &result
			return tea.Quit()
		}
		return msg
	}))
	_, err := program.Run()
	require.NoError(t, err)
	require.NotNil(t, loaded, "Init must dispatch its startup read")
	msg := *loaded
	require.NoError(t, msg.err)
	m, _ = m.cronSeenResult(msg)
	require.True(t, m.cronSeen.ready)
}

func TestCronSeenFillReachableWithTokens(t *testing.T) {
	m := cronSeenFixture(t)
	r := generated.CronRun{UID: "quiet-failure", JobUID: new(m.cron.data.jobs[0].UID), Status: "failed", Actor: "operator", UpdatedAt: snapshotFixedNow}
	m.cron.data.lastRuns = map[string]generated.CronRun{*r.JobUID: r}
	m = m.cronRemember([]generated.CronRun{r})
	m.cron.lists[cronRuns].query = "status:failed actor:operator"
	m = sendRune(m, 'u')
	require.Positive(t, m.cronUnseenCount())
	_, _, lines := cronScreen(m)
	found := false
	for _, line := range lines {
		if line.target.uid == r.UID {
			found = true
		}
	}
	require.True(t, found, "the unseen review must include failures counted from quiet job fills")
	m = sendKey(m, tea.KeyEnter)
	require.Equal(t, r.UID, m.cron.stack[len(m.cron.stack)-1].ref.uid)
}

func TestCronSeenRetainedFailureMarker(t *testing.T) {
	m := cronSeenFixture(t)
	old := m.cron.data.runs[0]
	old.UID, old.Status, old.Revision = "corrected-failure", "failed", 1
	old.EndedAt = new(snapshotFixedNow)
	fresh := old
	fresh.Status, fresh.Revision = "succeeded", 2
	m.cron.data.runs = nil
	m.cron.data.lastRuns = nil
	m.cron.data.lookup = map[string]generated.CronRun{old.UID: fresh}
	m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{old}, ready: true, older: true}
	m.cron.tab = cronRuns
	require.Zero(t, m.cronUnseenCount(), "newest loaded revision is successful")
	_, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
	require.NotContains(t, lines[0].text, "•", "zero unseen count must not show an unseen failure marker")
}

func TestCronSeenAsyncMergesAndDisablesWrites(t *testing.T) {
	m := cronSeenFixture(t)
	r := m.cron.data.runs[0]
	r.Status = "failed"
	m.cron.data.runs = []generated.CronRun{r}
	m = m.cronMarkSeen(r.UID)
	m, _ = m.cronSeenResult(cronSeenResultMsg{data: cronSeenStore{Version: 1, Projects: map[string]cronSeenProject{"other": {Runs: []string{"elsewhere"}}}}, write: true})
	require.Zero(t, m.cronUnseenCount())
	require.Contains(t, m.cronSeen.data.Projects, "other")
	m, _ = m.cronSeenResult(cronSeenResultMsg{err: fmt.Errorf("corrupt\nstate")})
	require.Error(t, m.cronSeen.err)
	require.Contains(t, stripANSI(renderCron(m)), "seen state unavailable: corrupt\\nstate")
	_, cmd := m.cronSeenSync()
	require.Nil(t, cmd, "failed load/write disables writes for the session")
	// Loading the file after an early in-memory mark never discards that mark.
	m = cronSeenFixture(t)
	m.cronSeen.ready = false
	m.cron.data.runs = []generated.CronRun{r}
	m = m.cronMarkSeen(r.UID)
	m, _ = m.cronSeenResult(cronSeenResultMsg{data: cronSeenStore{Version: 1, Projects: map[string]cronSeenProject{}}})
	require.Zero(t, m.cronUnseenCount())

	t.Run("corrupt persisted state leaves count unknown", func(t *testing.T) {
		m := cronSeenFixture(t)
		r := m.cron.data.runs[0]
		r.Status = "failed"
		m.cron.data.runs = []generated.CronRun{r}
		path := filepath.Join(t.TempDir(), "cron-seen.json")
		require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0600))
		m.cronSeen = cronSeenState{path: path}
		msg := m.cronSeenCommand(false)().(cronSeenResultMsg)
		require.Error(t, msg.err)
		m, _ = m.cronSeenResult(msg)
		screen := stripANSI(renderCron(m))
		require.Contains(t, screen, "seen state unavailable")
		require.NotContains(t, screen, "1 unseen failed", "unreadable persisted acknowledgements leave the count unknown")
	})
}
