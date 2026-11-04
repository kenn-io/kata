package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

// Contract: only ready availability diagnostics produce a banner; input order
// cannot change its priority or distinct-job count. Suggestions are not eligibility.
func drawCronRuntimeInventory(h *hegel.T) *cron.RuntimeInventory {
	state := hegel.Draw(h, hegel.SampledFrom([]string{"unknown", "warming", "ready"}))
	codes := hegel.Draw(h, hegel.Lists(hegel.SampledFrom([]string{"no_live_runtime", "pin_unmatched", "adapter_unmatched", "choice_unknown", "kata_version_skew"})))
	if len(codes) > 100 {
		codes = codes[:100]
	}
	inventory := &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: state}}
	for i, code := range codes {
		id := "job-a"
		if i%2 == 0 {
			id = "job-b"
		}
		if code == "no_live_runtime" {
			id = ""
		}
		inventory.Warnings = append(inventory.Warnings, cron.RuntimeWarning{Code: code, JobUID: id, Message: code + id})
	}
	return inventory
}
func TestCronRuntimeBannerProperties(t *testing.T) {
	t.Run("visibility", func(t *testing.T) {
		hegel.Test(t, func(h *hegel.T) {
			inventory := drawCronRuntimeInventory(h)
			want := false
			for _, w := range inventory.Warnings {
				if w.Code == "no_live_runtime" || w.Code == "pin_unmatched" || w.Code == "adapter_unmatched" {
					want = true
				}
			}
			_, show := cronRuntimeBanner(inventory, nil, false)
			if show != (inventory.Inventory.State == "ready" && want) {
				h.Fatal("banner violated readiness/code contract")
			}
		}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	})
	t.Run("priority", func(t *testing.T) {
		hegel.Test(t, func(h *hegel.T) {
			inventory := drawCronRuntimeInventory(h)
			inventory.Inventory.State = "ready"
			inventory.Warnings = append(inventory.Warnings, cron.RuntimeWarning{Code: "no_live_runtime"})
			text, show := cronRuntimeBanner(inventory, nil, false)
			if !show || !strings.HasPrefix(text, "No live runtime advertised") {
				h.Fatal("no-live warning lost priority")
			}
		}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	})
	t.Run("permutation", func(t *testing.T) {
		hegel.Test(t, func(h *hegel.T) {
			inventory := drawCronRuntimeInventory(h)
			one, show := cronRuntimeBanner(inventory, nil, false)
			slices.Reverse(inventory.Warnings)
			two, showTwo := cronRuntimeBanner(inventory, nil, false)
			if one != two || show != showTwo {
				h.Fatal("warning order changed banner")
			}
		}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	})
}
func TestCronRuntimeMessagesProperty(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		inventory := drawCronRuntimeInventory(h)
		expected := []string{}
		if inventory.Inventory.State == "ready" {
			for _, w := range inventory.Warnings {
				if w.JobUID == "job-a" {
					expected = append(expected, w.Message)
				}
			}
		}
		slices.Sort(expected)
		expected = slices.Compact(expected)
		got := cronRuntimeJobMessages(inventory, "job-a")
		if !slices.Equal(expected, got) {
			h.Fatalf("job messages %v want %v", got, expected)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
func TestCronRuntimeHelpersPreserveInputs(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		inventory := drawCronRuntimeInventory(h)
		jobs := []generated.CronJob{{Definition: generated.CronJobDefinitionResponse{Enabled: hegel.Draw(h, hegel.Booleans()), Action: generated.ActionResponse{Kind: hegel.Draw(h, hegel.SampledFrom([]string{"execute", "notify"}))}}}}
		before, _ := json.Marshal([]any{inventory, jobs})
		cronRuntimeBanner(inventory, jobs, true)
		cronRuntimeJobMessages(inventory, "job-a")
		after, _ := json.Marshal([]any{inventory, jobs})
		if string(before) != string(after) {
			h.Fatal("runtime display mutated inputs")
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
func TestCronRuntimeBannerCounts(t *testing.T) {
	inventory := &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}, Warnings: []cron.RuntimeWarning{{Code: "no_live_runtime"}}}
	jobs := []generated.CronJob{{Definition: generated.CronJobDefinitionResponse{Enabled: true, Action: generated.ActionResponse{Kind: "execute"}}}, {Definition: generated.CronJobDefinitionResponse{Enabled: false, Action: generated.ActionResponse{Kind: "execute"}}}, {Definition: generated.CronJobDefinitionResponse{Enabled: true, Action: generated.ActionResponse{Kind: "notify"}}}}
	text, show := cronRuntimeBanner(inventory, jobs, true)
	require.True(t, show)
	require.Contains(t, text, "1 enabled execute job.")
	require.NotContains(t, text, "will not run")
	for _, n := range []int{0, 2} {
		counted := append([]generated.CronJob{}, jobs[1:]...)
		for range n {
			counted = append(counted, jobs[0])
		}
		text, _ := cronRuntimeBanner(inventory, counted, true)
		require.Contains(t, text, fmt.Sprintf("%d enabled execute jobs.", n))
	}
	text, _ = cronRuntimeBanner(inventory, jobs, false)
	require.NotContains(t, text, "1 enabled")
	inventory.Warnings = []cron.RuntimeWarning{{Code: "pin_unmatched", JobUID: "a"}, {Code: "adapter_unmatched", JobUID: "a"}}
	text, _ = cronRuntimeBanner(inventory, jobs, true)
	require.Contains(t, text, "1 enabled execute job has no matching")
	inventory.Warnings = []cron.RuntimeWarning{{Code: "pin_unmatched", JobUID: "a"}, {Code: "adapter_unmatched", JobUID: "a"}, {Code: "pin_unmatched", JobUID: "b"}}
	text, _ = cronRuntimeBanner(inventory, jobs, true)
	require.Contains(t, text, "2 enabled execute jobs")
	text, _ = cronRuntimeBanner(inventory, jobs, false)
	require.NotContains(t, text, "2 enabled execute jobs")
	require.Contains(t, text, "matching live advertisement")
}
func TestCronRuntimeGoldens(t *testing.T) {
	for _, tc := range []struct {
		name, state, code string
		detail            bool
	}{{"cron_jobs_no_live_runtime_banner", "ready", "no_live_runtime", false}, {"cron_jobs_pin_unmatched_banner", "ready", "pin_unmatched", false}, {"cron_jobs_warming_no_banner", "warming", "no_live_runtime", false}, {"cron_job_detail_runtime_field", "ready", "pin_unmatched", true}} {
		t.Run(tc.name, func(t *testing.T) {
			defer snapshotInit(t)()
			m := cronFixture(t)
			id := m.cron.data.jobs[0].UID
			m.cron.runtimes = &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: tc.state}, Warnings: []cron.RuntimeWarning{{Code: tc.code, JobUID: id, Message: "no matching live advertisement for executor example-worker"}}}
			if tc.detail {
				m = sendKey(m, tea.KeyEnter)
			}
			rendered := renderCron(m)
			require.Equal(t, m.height, len(strings.Split(rendered, "\n")))
			assertGolden(t, tc.name, rendered)
			if tc.state == "ready" {
				for _, tab := range []rune{'2', '3', '4'} {
					m = sendRune(m, tab)
					if tc.code == "no_live_runtime" {
						require.Contains(t, renderCron(m), "No live runtime advertised")
					} else {
						require.Contains(t, renderCron(m), "live advertisement")
					}
				}
			}
		})
	}
}

type cronRuntimeTestAPI struct {
	KataAPI
	inventory *cron.RuntimeInventory
	err       error
	calls     int
}

func (a *cronRuntimeTestAPI) ListCronRuntimes(ctx context.Context, _ int64) (*cron.RuntimeInventory, error) {
	a.calls++
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Second {
		return nil, errors.New("missing short read budget")
	}
	return a.inventory, a.err
}
func runtimePollFixture(t *testing.T) (Model, *cronRuntimeTestAPI) {
	t.Helper()
	m := cronFixture(t)
	a := &cronRuntimeTestAPI{KataAPI: &cronReadTestAPI{KataAPI: &inboxTestAPI{}, data: *m.cron.data}, inventory: &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}}}
	m.api = a
	m.cron.runtimePoll = cronRuntimePoll{active: true, epoch: 7, seq: 3, out: true}
	return m, a
}
func TestCronRuntimeReadOrdering(t *testing.T) {
	m, _ := runtimePollFixture(t)
	ready := &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}}
	msg := cronRuntimeLoadedMsg{conn: m.connGen, scope: m.scopeGen, epoch: 7, seq: 3, inventory: ready}
	m, _ = updateModel(m, msg)
	require.Same(t, ready, m.cron.runtimes)
	m, _ = updateModel(m, cronLoadedMsg(*m.cron.data))
	require.Same(t, ready, m.cron.runtimes, "ordinary cron refresh overwrote inventory")
	stale := msg
	stale.seq = 2
	stale.inventory = nil
	m, _ = updateModel(m, stale)
	require.Same(t, ready, m.cron.runtimes)
	m.cron.runtimePoll.seq = 4
	m.cron.runtimePoll.out = true
	failed := msg
	failed.seq = 4
	failed.err = errors.New("older daemon")
	m, _ = updateModel(m, failed)
	require.Nil(t, m.cron.runtimes)
	m.scopeGen++
	m, _ = updateModel(m, msg)
	require.Nil(t, m.cron.runtimes, "stale scope restored runtime evidence")
}
func TestCronRuntimePollViewLifetime(t *testing.T) {
	m, a := runtimePollFixture(t)
	oldEpoch := m.cron.runtimePoll.epoch
	m.cron.runtimes = a.inventory
	m, _ = updateModel(m, keyRune('1'))
	require.False(t, m.cron.runtimePoll.active)
	m, cmd := updateModel(m, keyRune('2'))
	require.Nil(t, m.cron.runtimes)
	require.Greater(t, m.cron.runtimePoll.epoch, oldEpoch)
	messages := runCmdTree(cmd)
	require.Equal(t, 1, a.calls)
	for _, msg := range messages {
		m, _ = updateModel(m, msg)
	}
	current := m.cron.runtimes
	m, _ = updateModel(m, cronRuntimeLoadedMsg{conn: m.connGen, scope: m.scopeGen, epoch: oldEpoch, seq: 3, err: errors.New("stale")})
	require.Same(t, current, m.cron.runtimes)
	require.False(t, m.cron.runtimePoll.out)
}

// Contract: the existing minute clock refreshes only runtime evidence, with one
// read in flight. The same command keeps the ordinary day clock alive.
func TestCronRuntimePollAfterMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a := runtimePollFixture(t)
		m.cron.runtimePoll.out = false
		before := time.Now()
		msg := dayTick()()
		require.Equal(t, time.Minute, time.Since(before))
		m, cmd := updateModel(m, msg)
		require.True(t, m.cron.runtimePoll.out, "minute clock must start an optional runtime read")
		seq := m.cron.runtimePoll.seq
		m, _ = updateModel(m, dayTickMsg{})
		require.Equal(t, seq, m.cron.runtimePoll.seq, "ticks coalesce while a read is in flight")
		for _, result := range runCmdTree(cmd) {
			if _, ok := result.(cronRuntimeLoadedMsg); ok {
				var replacement tea.Cmd
				m, replacement = updateModel(m, result)
				for _, fresh := range runCmdTree(replacement) {
					m, _ = updateModel(m, fresh)
				}
			}
		}
		require.Equal(t, 2, a.calls)
		require.False(t, m.cron.runtimePoll.out)
		require.Same(t, a.inventory, m.cron.runtimes)
	})
}

// Contract: the aggregate no-live advisory appears once, in the banner. The
// detail row contains only the selected job's warnings and vanishes if empty.
func TestCronRuntimeDetailDoesNotRepeatAggregate(t *testing.T) {
	defer snapshotInit(t)()
	for _, specific := range []bool{false, true} {
		m := cronFixture(t)
		id := m.cron.data.jobs[0].UID
		m.cron.runtimes = &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}, Warnings: []cron.RuntimeWarning{{Code: "no_live_runtime", Message: "no live runtime advertisement in this daemon's inventory"}}}
		if specific {
			m.cron.runtimes.Warnings = append(m.cron.runtimes.Warnings, cron.RuntimeWarning{Code: "pin_unmatched", JobUID: id, Message: "no matching live advertisement for executor example-worker"})
		}
		m = sendKey(m, tea.KeyEnter)
		rendered := stripANSI(renderCron(m))
		require.Equal(t, 1, strings.Count(rendered, "No live runtime advertised"))
		require.NotContains(t, rendered, "no live runtime advertisement in this daemon's inventory")
		if specific {
			require.Contains(t, rendered, "Runtime:")
			require.Contains(t, rendered, "no matching live advertisement")
		} else {
			require.NotContains(t, rendered, "Runtime:")
		}
	}
}

// Optional evidence may finish before the ordinary jobs request. Its aggregate
// warning remains useful without inventing an execute-job count.
func TestCronRuntimeBannerBeforeJobs(t *testing.T) {
	m := cronFixture(t)
	m.cron.data = nil
	m.cron.runtimes = &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}, Warnings: []cron.RuntimeWarning{{Code: "no_live_runtime"}}}
	rendered := renderCron(m)
	require.Contains(t, rendered, "No live runtime advertised")
	require.NotContains(t, rendered, "0 enabled execute jobs")
}

func TestCronRuntimeNarrowRender(t *testing.T) {
	defer snapshotInit(t)()
	for _, width := range []int{40, 80, 120} {
		m := cronFixture(t)
		m.width = width
		m.cron.runtimes = &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}, Warnings: []cron.RuntimeWarning{{Code: "no_live_runtime"}}}
		rendered := renderCron(m)
		lines := strings.Split(rendered, "\n")
		require.Len(t, lines, m.height)
		require.Contains(t, stripANSI(lines[3]), "No live runtime advertised")
	}
}

// A manual refresh during an in-flight read queues one replacement, even if
// several refreshes arrive. Old evidence cannot satisfy the newer intent.
func TestCronRuntimeRefreshDuringRead(t *testing.T) {
	for _, failed := range []bool{false, true} {
		m, a := runtimePollFixture(t)
		old := cronRuntimeLoadedMsg{conn: m.connGen, scope: m.scopeGen, epoch: 7, seq: 3,
			inventory: &cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}, Warnings: []cron.RuntimeWarning{{Code: "no_live_runtime"}}}}
		if failed {
			old.err = errors.New("offline")
		}
		for range 2 {
			var cmd tea.Cmd
			m, cmd = updateModel(m, keyRune('r'))
			for _, msg := range runCmdTree(cmd) {
				m, _ = updateModel(m, msg)
			}
		}
		require.Zero(t, a.calls)
		m, cmd := updateModel(m, old)
		for _, msg := range runCmdTree(cmd) {
			m, _ = updateModel(m, msg)
		}
		require.Equal(t, 1, a.calls)
		require.Same(t, a.inventory, m.cron.runtimes)
		require.False(t, m.cron.runtimePoll.out)
		_, cmd = updateModel(m, old)
		require.Nil(t, cmd, "a duplicate stale response cannot restart the read")
	}
}
