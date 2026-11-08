package tui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Project state uses its own optional envelope: old daemons can omit fields
// without hiding cron definitions, and an unavailable read is distinguishable.
func TestCronProjectPauseClient(t *testing.T) {
	for _, tc := range []struct {
		name, raw       string
		paused, invalid bool
		controls        []string
		status          int
	}{
		{name: "older", raw: `{"project_uid":"example"}`, status: 200},
		{name: "clear", raw: `{"project_pause":null,"controls":["job_pause","project_pause"]}`, controls: []string{"job_pause", "project_pause"}, status: 200},
		{name: "paused", raw: `{"project_pause":{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"Maintenance"},"controls":["project_pause"]}`, paused: true, controls: []string{"project_pause"}, status: 200},
		{name: "malformed", raw: `{"project_pause":{"invalid":true,"reason":""},"controls":["project_pause"]}`, paused: true, invalid: true, controls: []string{"project_pause"}, status: 200},
		{name: "unavailable", raw: `{"error":{"code":"unavailable","message":"unavailable"}}`, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/projects/7/cron/capabilities", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.raw))
			}))
			t.Cleanup(server.Close)
			c := NewClient(server.URL, server.Client())
			state, err := c.readCronProjectState(t.Context(), 7)
			if tc.status != 200 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.paused, state.pause != nil)
			require.Equal(t, tc.controls, state.controls)
			if tc.paused {
				require.Equal(t, tc.invalid, state.pause.Invalid)
			}
		})
	}
}

func TestCronProjectPauseBanner(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		for _, tab := range []rune{'2', '3', '4'} {
			t.Run(fmt.Sprintf("invalid=%t/tab=%c", invalid, tab), func(t *testing.T) {
				m := cronFixture(t)
				metadata := db.JSONBlob(`{"cron.pause":{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"Maintenance\nwindow"}}`)
				if invalid {
					metadata = `{"cron.pause":false}`
				}
				m.cron.data.project = cronProjectState{pause: db.CronProjectPauseFromMetadata(metadata)}
				m = sendRune(m, tab)
				rendered := ansi.Strip(renderCron(m))
				require.Contains(t, rendered, "Project cron paused")
				if invalid {
					require.Contains(t, rendered, "invalid policy")
				} else {
					require.Contains(t, rendered, "operator")
					require.Contains(t, rendered, "Maintenance")
				}
			})
		}
	}
	m := cronFixture(t)
	require.NotContains(t, ansi.Strip(renderCron(m)), "Project cron paused")
}

// Failed refreshes retain visible policy and display their error; an absent
// pause from a successful refresh clears the banner.
func TestCronProjectPauseRefresh(t *testing.T) {
	m := cronFixture(t)
	m.cron.data.project = cronProjectState{pause: &db.CronProjectPause{PausedBy: "operator", Reason: "Maintenance"}}
	incoming := *m.cron.data
	incoming.project = cronProjectState{}
	incoming.projectErr = errors.New("offline")
	m = m.cronLoaded(cronLoadedMsg(incoming))
	rendered := ansi.Strip(renderCron(m))
	require.Contains(t, rendered, "Project cron paused")
	require.Contains(t, rendered, "project policy: offline")
	incoming.projectErr = nil
	m = m.cronLoaded(cronLoadedMsg(incoming))
	require.NotContains(t, ansi.Strip(renderCron(m)), "Project cron paused")
}

func TestCronProjectPauseFetch(t *testing.T) {
	m := cronFixture(t)
	reader := &cronReadTestAPI{data: *m.cron.data}
	reader.data.project = cronProjectState{pause: &db.CronProjectPause{Invalid: true}}
	m.api = reader
	message := m.fetchCron()()
	loaded, ok := message.(cronLoadedMsg)
	require.True(t, ok)
	require.NotNil(t, loaded.project.pause)
	require.True(t, loaded.project.pause.Invalid)
}

func TestSnapshot_CronProjectPaused(t *testing.T) {
	defer snapshotInit(t)()
	for _, width := range []int{80, 120} {
		m := cronFixture(t)
		m.width = width
		m.cron.data.project = cronProjectState{pause: &db.CronProjectPause{PausedBy: "operator", Reason: "Maintenance"}}
		assertGolden(t, fmt.Sprintf("cron-jobs-project-paused-%d", width), m.View().Content)
	}
}

// Contract: ordinary project metadata events refresh the cached policy, while
// another project's events leave the current cron snapshot usable.
func TestCronProjectPauseMetadataEventRefresh(t *testing.T) {
	for _, projectID := range []int64{7, 8} {
		t.Run(fmt.Sprintf("project=%d", projectID), func(t *testing.T) {
			m := cronFixture(t)
			m.view = viewCron
			m.scope.projectID = 7
			m.cron.conn, m.cron.scope = m.connGen, m.scopeGen
			m.cron.data.events = m.cron.events
			m.cron.out = false
			reader := &cronReadTestAPI{data: *m.cron.data}
			reader.data.project = cronProjectState{pause: &db.CronProjectPause{PausedBy: "operator", Reason: "Maintenance"}}
			m.api = reader
			next, _ := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: projectID})
			m = next.(Model)
			m, fetch := m.cronSync()
			if projectID != 7 {
				require.Nil(t, fetch)
				return
			}
			require.NotNil(t, fetch, "project policy event must invalidate cron snapshot")
			m = m.cronLoaded(fetch().(cronLoadedMsg))
			require.Contains(t, ansi.Strip(renderCron(m)), "Maintenance")
		})
	}
}
