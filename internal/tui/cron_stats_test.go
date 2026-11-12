package tui

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func cronDetailText(m Model) string {
	_, _, lines := cronScreen(m)
	return cronHistoryLinesText(lines)
}

func cronStatsLine(t *testing.T, m Model) string {
	t.Helper()
	for line := range strings.SplitSeq(cronDetailText(m), "\n") {
		if strings.HasPrefix(line, "30d:") {
			return line
		}
	}
	t.Fatalf("no 30d line in job detail:\n%s", cronDetailText(m))
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
		require.Equal(t, "30d:      42 runs · 92% ok · median 3m12s · p95 8m40s · failing ×3", cronStatsLine(t, m))
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
			require.NotContains(t, stripANSI(m.viewBody()), "unavailable: see error below")
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
		require.Equal(t, "30d:      unavailable: offline", cronStatsLine(t, m))
		requireCronJobDetailIntact(t, m)
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
		name string
		edit func(*generated.CronRunStatsResponseBody)
		want string
	}{
		{"spec example", func(*generated.CronRunStatsResponseBody) {}, "42 runs · 92% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"no streak", func(s *generated.CronRunStatsResponseBody) { s.ConsecutiveFailures = 0 }, "42 runs · 92% ok · median 3m12s · p95 8m40s"},
		{"no runs", func(s *generated.CronRunStatsResponseBody) {
			s.Scanned, s.SuccessRate, s.DurationSeconds, s.ConsecutiveFailures = 0, nil, nil, 0
		}, "0 runs"},
		{"one run", func(s *generated.CronRunStatsResponseBody) { s.Scanned = 1 }, "1 run · 92% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"truncated", func(s *generated.CronRunStatsResponseBody) { s.Scanned, s.Truncated = 10000, true }, "10000+ runs · 92% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"rate floors", func(s *generated.CronRunStatsResponseBody) { s.SuccessRate = new(0.9999) }, "42 runs · 99% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"rate exact", func(s *generated.CronRunStatsResponseBody) { s.SuccessRate = new(0.29) }, "42 runs · 29% ok · median 3m12s · p95 8m40s · failing ×3"},
		{"long durations", func(s *generated.CronRunStatsResponseBody) {
			s.DurationSeconds = &generated.DurationStats{Count: 2, Min: 0.4, Median: 41, P95: 3725.6, Max: 3725.6}
		}, "42 runs · 92% ok · median 41s · p95 1h2m6s · failing ×3"},
		{"duration beyond a time.Duration", func(s *generated.CronRunStatsResponseBody) {
			s.DurationSeconds = &generated.DurationStats{Count: 1, Median: 1.89342144e10, P95: 1.89342144e10}
		}, "42 runs · 92% ok · median 5259504h · p95 5259504h · failing ×3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := full
			tc.edit(&s)
			require.Equal(t, tc.want, cronStatsText(s))
		})
	}
}

func TestCronClientRunStats(t *testing.T) {
	for _, version := range []string{"0.46.0", "0.47.0"} {
		t.Run(version, func(t *testing.T) {
			reads := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					respondJSON(t, w, map[string]any{"api_schema_version": version})
					return
				}
				reads++
				require.Equal(t, "/api/v1/projects/7/cron/runs/stats", r.URL.Path)
				require.Equal(t, map[string][]string{"job_uid": {cronStatsJobUID}}, map[string][]string(r.URL.Query()))
				respondJSON(t, w, cronStatsFixture(42))
			})
			stats, err := c.CronRunStats(t.Context(), 7, cronStatsJobUID)
			if version == "0.46.0" {
				var tooOld apiVersionError
				require.ErrorAs(t, err, &tooOld)
				require.Zero(t, reads)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, reads)
			require.Equal(t, int64(42), stats.Scanned)
			require.Equal(t, int64(3), stats.ConsecutiveFailures)
		})
	}
}
