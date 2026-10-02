package tui

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
)

const tuiAppOpenedBody = `{"event":"app_opened","properties":{"surface":"tui"}}`

type recordingCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *recordingCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, string(bytes.TrimSpace(body)))
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"queued"}`))
}

func TestReportAppOpenedReachesDaemonCapture(t *testing.T) {
	t.Run("recorded body", func(t *testing.T) {
		recorder := &recordingCapture{}
		env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.TelemetryCapture = recorder })

		require.NoError(t, NewClient(env.URL, env.HTTP).ReportAppOpened(t.Context()))

		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		require.Len(t, recorder.bodies, 1)
		assert.JSONEq(t, tuiAppOpenedBody, recorder.bodies[0])
	})
	t.Run("daemon allowlist", func(t *testing.T) {
		reporter, err := telemetry.NewReporter(telemetry.Options{})
		require.NoError(t, err)
		env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.TelemetryCapture = telemetry.CaptureHandler(reporter) })

		assert.NoError(t, NewClient(env.URL, env.HTTP).ReportAppOpened(t.Context()))
	})
}

// runCmd runs cmd and every command a batch result carries, like Bubble Tea would.
func runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, inner := range batch {
			runCmd(inner)
		}
	}
}

func TestInitReportsAppOpenedOncePerLaunch(t *testing.T) {
	var reports atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/ui/telemetry" {
			reports.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	init := bootInit{scope: scope{projectID: 7, projectName: "example-project"}, view: viewList}
	m := buildRunModel(Options{}, NewClient(srv.URL, srv.Client()), init)

	batch, ok := m.Init()().(tea.BatchMsg)
	require.True(t, ok)
	for _, cmd := range batch[1:] { // batch[0] waits on the SSE channel
		runCmd(cmd)
	}
	assert.Equal(t, int32(1), reports.Load())

	m, cmd := m.installDaemonConnection(daemonConnection{api: NewClient(srv.URL, srv.Client()), init: init})
	runCmd(cmd)
	m, cmd = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: m.connGen})
	runCmd(cmd)
	assert.Equal(t, int32(1), reports.Load(), "daemon switches and capability refetches must not report again")
}

type noAppOpenedAPI struct{ KataAPI }

func TestReportAppOpenedFailureIsSilent(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	readOnly := testenv.New(t, testenv.WithInsecureReadonly())
	for _, test := range []struct {
		name   string
		client *Client
	}{
		{name: "insecure readonly daemon", client: NewClient(readOnly.URL, readOnly.HTTP)},
		{name: "daemon without the route", client: NewClient(notFound.URL, notFound.Client())},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, test.client.ReportAppOpened(t.Context()))
			m := initialModel(Options{})
			m.api = newUndoClient(test.client)

			cmd := m.reportAppOpened()
			require.NotNil(t, cmd)
			assert.Nil(t, cmd())
			assert.Nil(t, m.toast)
		})
	}

	m := initialModel(Options{})
	m.api = noAppOpenedAPI{}
	assert.Nil(t, m.reportAppOpened())
}
