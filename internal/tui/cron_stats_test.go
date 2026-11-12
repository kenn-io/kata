package tui

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronStatsJobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// cronStatsFixture is the spec's synthetic 30-day response.
func cronStatsFixture(scanned int64) *generated.CronRunStatsResponseBody {
	return &generated.CronRunStatsResponseBody{
		ProjectUID: "01J00000000000000000000001", JobUID: new(cronStatsJobUID),
		Window:              generated.CronRunStatsWindow{Since: snapshotFixedNow.AddDate(0, 0, -30), Until: snapshotFixedNow},
		Scanned:             scanned,
		ByStatus:            map[string]int64{"running": 1, "succeeded": 36, "failed": 3, "cancelled": 2, "unknown": 0},
		SuccessRate:         new(0.9231),
		DurationSeconds:     &generated.DurationStats{Count: 39, Min: 41, Median: 192, P95: 520, Max: 611},
		ConsecutiveFailures: 3,
	}
}

// cronStatsModel is the job fixture with a read double that answers
// statistics per job UID.
func cronStatsModel(t *testing.T) (Model, *cronReadTestAPI) {
	t.Helper()
	m := cronFixture(t)
	m.sseCh = nil
	a := &cronReadTestAPI{KataAPI: &inboxTestAPI{}, data: *m.cron.data}
	a.stats = map[string]*generated.CronRunStatsResponseBody{cronStatsJobUID: cronStatsFixture(42)}
	m.api = a
	return m, a
}

// deliverCron runs a command tree and feeds back the cron results it produced.
func deliverCron(m Model, cmd tea.Cmd) Model {
	for _, msg := range runCmdTree(cmd) {
		switch msg.(type) {
		case cronStatsMsg, cronLoadedMsg:
			m, _ = updateModel(m, msg)
		}
	}
	return m
}

// runCmdTreeConcurrently runs each command of a tree in its own goroutine,
// as Bubble Tea does, and sends every result to out.
func runCmdTreeConcurrently(cmd tea.Cmd, out chan<- tea.Msg) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, inner := range batch {
				runCmdTreeConcurrently(inner, out)
			}
			return
		}
		out <- msg
	}()
}

func cronDetailText(m Model) string {
	_, _, lines := cronScreen(m)
	return cronHistoryLinesText(lines)
}

func cronStatsLine(t *testing.T, m Model) string {
	t.Helper()
	for line := range strings.SplitSeq(cronDetailText(m), "\n") {
		if strings.HasPrefix(line, "30d:") || strings.HasPrefix(line, "Since:") {
			return line
		}
	}
	t.Fatalf("no statistics line in job detail:\n%s", cronDetailText(m))
	return ""
}

func requireCronJobDetailIntact(t *testing.T, m Model) {
	t.Helper()
	text := cronDetailText(m)
	for _, want := range []string{"State:    enabled", "Schedule: 0 7 * * *", "Workflow: Summary flow", "Definition", "Sample: 1 observation"} {
		require.Contains(t, text, want)
	}
}

func TestCronJobStatsLine(t *testing.T) {
	t.Run("loads when the detail opens", func(t *testing.T) {
		m, a := cronStatsModel(t)
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		require.Equal(t, "30d:      loading…", cronStatsLine(t, m))
		m = deliverCron(m, cmd)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      42 runs"), cronStatsLine(t, m))
		require.Equal(t, []string{cronStatsJobUID}, a.statsUIDs)
		requireCronJobDetailIntact(t, m)
		_, cmd = m.cronSync()
		require.Nil(t, cmd, "a loaded line is not re-read")
	})
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unsupported daemon", apiVersionError{"run statistics require daemon API 0.47.0 or newer"}, "30d:      needs daemon API 0.47.0 or newer"},
		{"failed read", errors.New("stats read failed"), "30d:      unavailable: stats read failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a := cronStatsModel(t)
			a.statsErr = tc.err
			m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			m = deliverCron(m, cmd)
			require.Equal(t, tc.want, cronStatsLine(t, m))
			requireCronJobDetailIntact(t, m)
		})
	}
	t.Run("ignores a result for a job no longer open", func(t *testing.T) {
		m, a := cronStatsModel(t)
		other := m.cron.data.jobs[0]
		other.UID, other.Name, other.ID = "01ARZ3NDEKTSV4RRFFQ69G5FBA", "Weekly summary", 9
		data := *m.cron.data
		data.jobs = append(slices.Clone(data.jobs), other)
		m, _ = updateModel(m, cronLoadedMsg(data))
		a.data = data
		a.stats[other.UID] = cronStatsFixture(7)
		m, first := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = sendRune(m, 'h')
		m = sendRune(m, 'j')
		m, second := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		require.Equal(t, other.UID, m.cron.stack[0].ref.uid)
		m = deliverCron(m, second)
		m = deliverCron(m, first)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      7 runs"), cronStatsLine(t, m))
	})
	t.Run("rereads when the same job is reopened", func(t *testing.T) {
		m, a := cronStatsModel(t)
		a.statsErr = errors.New("offline")
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = deliverCron(m, cmd)
		require.Equal(t, "30d:      unavailable: offline", cronStatsLine(t, m))
		// The service recovers: reopening reads again instead of keeping the notice.
		a.statsErr = nil
		m = sendRune(m, 'h')
		m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		require.Equal(t, "30d:      loading…", cronStatsLine(t, m))
		m = deliverCron(m, cmd)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      42 runs"), cronStatsLine(t, m))
		// A reply that arrives after leaving is not shown on return.
		a.stats[cronStatsJobUID] = cronStatsFixture(43)
		m = sendRune(m, 'h')
		m, late := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = sendRune(m, 'h')
		m = deliverCron(m, late)
		m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		require.Equal(t, "30d:      loading…", cronStatsLine(t, m))
		m = deliverCron(m, cmd)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      43 runs"), cronStatsLine(t, m))
		require.Len(t, a.statsUIDs, 4)
	})
	t.Run("refreshes with the run refresh", func(t *testing.T) {
		m, a := cronStatsModel(t)
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = deliverCron(m, cmd)
		a.stats[cronStatsJobUID] = cronStatsFixture(43)
		m, cmd = updateModel(m, keyRune('r'))
		require.NotNil(t, cmd)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      42 runs"), "the old line stays during the refresh")
		m = deliverCron(m, cmd)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      43 runs"), cronStatsLine(t, m))
		require.Equal(t, []string{cronStatsJobUID, cronStatsJobUID}, a.statsUIDs)
		a.statsErr = errors.New("offline")
		m, cmd = updateModel(m, keyRune('r'))
		m = deliverCron(m, cmd)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      unavailable:"), "a failed refresh replaces the line")
	})
	t.Run("opens during a stalled list read", func(t *testing.T) {
		m, a := cronStatsModel(t)
		m, stalled := updateModel(m, keyRune('r'))
		require.NotNil(t, stalled)
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		require.NotNil(t, cmd, "opening the detail reads statistics without waiting for the list read")
		m = deliverCron(m, cmd)
		require.Zero(t, a.calls, "only the statistics were read")
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      42 runs"), cronStatsLine(t, m))
		_, cmd = updateModel(m, runCmdTree(stalled)[0])
		require.Nil(t, cmd, "the list answer does not read the statistics again")
		require.Len(t, a.statsUIDs, 1)
	})
	t.Run("a newer refresh supersedes the older reply", func(t *testing.T) {
		m, a := cronStatsModel(t)
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = deliverCron(m, cmd)
		a.stats[cronStatsJobUID] = cronStatsFixture(43)
		m, older := updateModel(m, keyRune('r'))
		var list, stats tea.Msg
		for _, msg := range runCmdTree(older) {
			switch msg.(type) {
			case cronLoadedMsg:
				list = msg
			case cronStatsMsg:
				stats = msg
			}
		}
		require.NotNil(t, list)
		require.NotNil(t, stats)
		a.stats[cronStatsJobUID] = cronStatsFixture(44)
		// A second refresh arrives while the first list read is still out.
		m, _ = updateModel(m, keyRune('r'))
		m, _ = updateModel(m, stats)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      42 runs"), "the superseded reply is not shown: %s", cronStatsLine(t, m))
		m, next := updateModel(m, list)
		require.NotNil(t, next)
		m = deliverCron(m, next)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      44 runs"), cronStatsLine(t, m))
	})
	t.Run("reads only a job the detail can show", func(t *testing.T) {
		m, a := cronStatsModel(t)
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = deliverCron(m, cmd)
		a.data.jobs = nil
		m, cmd = updateModel(m, keyRune('r'))
		m = deliverCron(m, cmd)
		require.Contains(t, stripANSI(m.viewBody()), "no longer in the loaded data")
		reads := len(a.statsUIDs)
		m, cmd = updateModel(m, keyRune('r'))
		deliverCron(m, cmd)
		require.Len(t, a.statsUIDs, reads, "no read for a job absent from the loaded data")
	})
	t.Run("a stalled read does not hold the run refresh", func(t *testing.T) {
		m, a := cronStatsModel(t)
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = deliverCron(m, cmd)
		a.data.jobs = slices.Clone(a.data.jobs)
		a.data.jobs[0].Name = "New title"
		a.stats[cronStatsJobUID] = cronStatsFixture(43)
		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		t.Cleanup(release)
		a.statsGate = gate
		m, cmd = updateModel(m, keyRune('r'))
		out := make(chan tea.Msg, 8)
		runCmdTreeConcurrently(cmd, out)
		select {
		case msg := <-out:
			loaded, ok := msg.(cronLoadedMsg)
			require.True(t, ok, "the run refresh answers first, got %T", msg)
			m, _ = updateModel(m, loaded)
		case <-time.After(2 * time.Second):
			t.Fatal("the run refresh waited for the statistics read")
		}
		require.Contains(t, stripANSI(m.viewBody()), "New title")
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      42 runs"), cronStatsLine(t, m))
		release()
		m, _ = updateModel(m, <-out)
		require.True(t, strings.HasPrefix(cronStatsLine(t, m), "30d:      43 runs"), cronStatsLine(t, m))
	})
	t.Run("fits the cron detail widths", func(t *testing.T) {
		truncated := cronStatsFixture(10000)
		truncated.Truncated, truncated.Window.Since = true, snapshotFixedNow.AddDate(0, 0, -7)
		for _, tc := range []struct {
			name string
			body *generated.CronRunStatsResponseBody
			err  error
			want string
		}{
			{"loaded", cronStatsFixture(42), nil, "30d:42runs·92%ok·median3m12s·p958m40s·failing×3"},
			{"since", truncated, nil, "Since:04-2412:00·10000+runs·92%ok·median3m12s·p958m40s·failing×3"},
			{"unavailable", nil, errors.New("Get http://daemon.example:7777/api/v1/projects/7/cron/runs/stats: connection refused"), "30d:unavailable:Gethttp://daemon.example:7777/api/v1/projects/7/cron/runs/stats:connectionrefused"},
			{"unsupported", nil, apiVersionError{"run statistics require daemon API 0.47.0 or newer"}, "30d:needsdaemonAPI0.47.0ornewer"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m, a := cronStatsModel(t)
				a.stats[cronStatsJobUID], a.statsErr = tc.body, tc.err
				m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				m = deliverCron(m, cmd)
				for _, width := range []int{12, 24, 40, 80, 120} {
					m.width = width + 2
					text := cronDetailText(m)
					for line := range strings.SplitSeq(text, "\n") {
						require.LessOrEqual(t, ansi.StringWidth(line), width, line)
					}
					require.Contains(t, strings.Join(strings.Fields(text), ""), tc.want)
				}
			})
		}
	})
	t.Run("workflow detail has no line", func(t *testing.T) {
		m, a := cronStatsModel(t)
		m = sendRune(m, '4')
		m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = deliverCron(m, cmd)
		require.NotContains(t, cronDetailText(m), "30d:")
		require.Empty(t, a.statsUIDs)
	})
}

func TestCronStatsText(t *testing.T) {
	full := *cronStatsFixture(42)
	for _, tc := range []struct {
		name         string
		edit         func(*generated.CronRunStatsResponseBody)
		label, value string
	}{
		{"spec example", func(*generated.CronRunStatsResponseBody) {}, "30d", "42 runs · 92% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"no streak", func(s *generated.CronRunStatsResponseBody) { s.ConsecutiveFailures = 0 }, "30d", "42 runs · 92% ok · median 3m12s · p95 8m40s"},
		{"no runs", func(s *generated.CronRunStatsResponseBody) {
			s.Scanned, s.SuccessRate, s.DurationSeconds, s.ConsecutiveFailures = 0, nil, nil, 0
			s.ByStatus = map[string]int64{"running": 0, "succeeded": 0, "failed": 0, "cancelled": 0, "unknown": 0}
		}, "30d", "0 runs"},
		{"one run", func(s *generated.CronRunStatsResponseBody) {
			s.Scanned, s.SuccessRate, s.ConsecutiveFailures = 1, new(1.0), 0
			s.ByStatus = map[string]int64{"running": 0, "succeeded": 1, "failed": 0, "cancelled": 0, "unknown": 0}
			s.DurationSeconds = &generated.DurationStats{Count: 1, Min: 192, Median: 192, P95: 192, Max: 192}
		}, "30d", "1 run · 100% ok · median 3m12s · p95 3m12s"},
		// A truncated scan covers only the span since its oldest run, so the
		// label names that start instead of claiming 30 days.
		{"truncated", func(s *generated.CronRunStatsResponseBody) {
			s.Scanned, s.Truncated, s.SuccessRate, s.ConsecutiveFailures = 10000, true, new(0.995), 0
			s.Window.Since = snapshotFixedNow.AddDate(0, 0, -7)
			s.ByStatus = map[string]int64{"running": 0, "succeeded": 9950, "failed": 50, "cancelled": 0, "unknown": 0}
			s.DurationSeconds = &generated.DurationStats{Count: 10000, Min: 41, Median: 192, P95: 520, Max: 611}
		}, "Since", "04-24 12:00 · 10000+ runs · 99% ok · median 3m12s · p95 8m40s"},
		{"rate floors", func(s *generated.CronRunStatsResponseBody) { s.SuccessRate = new(0.9999) }, "30d", "42 runs · 99% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"rate exact", func(s *generated.CronRunStatsResponseBody) { s.SuccessRate = new(0.29) }, "30d", "42 runs · 29% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"long durations", func(s *generated.CronRunStatsResponseBody) {
			s.DurationSeconds = &generated.DurationStats{Count: 2, Min: 0.4, Median: 41, P95: 3725.6, Max: 3725.6}
		}, "30d", "42 runs · 92% ok · median 41s · p95 1h2m6s · failing ×3"},
		{"duration beyond a time.Duration", func(s *generated.CronRunStatsResponseBody) {
			s.DurationSeconds = &generated.DurationStats{Count: 1, Median: 1.89342144e10, P95: 1.89342144e10}
		}, "30d", "42 runs · 92% ok · median 5259504h0m0s · p95 5259504h0m0s · failing ×3"},
		// Past the time.Duration bound the value keeps whole seconds.
		{"duration across the time.Duration bound", func(s *generated.CronRunStatsResponseBody) {
			s.DurationSeconds = &generated.DurationStats{Count: 2, Median: 9223372036, P95: 9223372037}
		}, "30d", "42 runs · 92% ok · median 2562047h47m16s · p95 2562047h47m17s · failing ×3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := full
			tc.edit(&s)
			label, value := cronStatsText(s)
			require.Equal(t, tc.label, label)
			require.Equal(t, tc.value, value)
		})
	}
}

func TestCronClientRunStats(t *testing.T) {
	for _, version := range []string{"0.46.0", "0.47.0"} {
		t.Run(version, func(t *testing.T) {
			probes, reads := 0, 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					probes++
					respondJSON(t, w, map[string]any{"api_schema_version": version})
					return
				}
				reads++
				require.Equal(t, "/api/v1/projects/7/cron/runs/stats", r.URL.Path)
				require.Equal(t, map[string][]string{"job_uid": {cronStatsJobUID}}, map[string][]string(r.URL.Query()))
				respondJSON(t, w, cronStatsFixture(42))
			})
			for range 2 {
				stats, err := c.CronRunStats(t.Context(), 7, cronStatsJobUID)
				if version == "0.46.0" {
					var tooOld apiVersionError
					require.ErrorAs(t, err, &tooOld)
					continue
				}
				require.NoError(t, err)
				require.Equal(t, int64(42), stats.Scanned)
				require.Equal(t, int64(3), stats.ConsecutiveFailures)
			}
			if version == "0.46.0" {
				require.Zero(t, reads)
				require.Equal(t, 2, probes, "an older daemon is probed again, since it may be upgraded")
				return
			}
			require.Equal(t, 2, reads)
			require.Equal(t, 1, probes, "a supported daemon is probed once per client")
		})
	}
	t.Run("failed version check", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		})
		_, err := c.CronRunStats(t.Context(), 7, cronStatsJobUID)
		require.Error(t, err)
		var tooOld apiVersionError
		require.False(t, errors.As(err, &tooOld))
		require.NotContains(t, err.Error(), "require daemon API", "a failed check does not blame the daemon version")
		require.Contains(t, err.Error(), "check daemon API version")
	})
	t.Run("other features keep their failed check text", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		})
		err := c.CheckDateFilter(t.Context())
		require.ErrorContains(t, err, "date filters require daemon API 0.36.0 or newer: ")
	})
}
