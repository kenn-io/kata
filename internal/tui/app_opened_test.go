package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
)

type recordingTelemetry struct {
	mu       sync.Mutex
	captured []map[string]any
}

func (*recordingTelemetry) EventAllowed(event string) bool {
	return event == "app_opened" || event == "session_ended" || event == "screen_viewed"
}
func (*recordingTelemetry) Enabled() bool { return true }
func (r *recordingTelemetry) Capture(_ string, properties map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captured = append(r.captured, properties)
	return nil
}

func TestReportAppOpenedReachesDaemonCapture(t *testing.T) {
	recorder := &recordingTelemetry{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = recorder })

	c := NewClient(env.URL, env.HTTP)
	require.NoError(t, c.ReportAppOpened(t.Context()))
	require.NoError(t, c.ReportSessionEnded(2*time.Minute))

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	assert.Equal(t, []map[string]any{{"surface": "tui"}, {"surface": "tui", "duration_bucket": "1_to_5m"}}, recorder.captured)
}

func TestReportSessionEndedBoundsDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
		c := NewClient("http://127.0.0.1:7777", hc)
		started := time.Now()
		assert.Error(t, c.ReportSessionEnded(time.Minute))
		assert.Equal(t, time.Second, time.Since(started))
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
			var body struct {
				Event string `json:"event"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Event == "app_opened" {
				reports.Add(1)
			}
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

func TestScreenViewsThroughModelAndDaemon(t *testing.T) {
	recorder := &recordingTelemetry{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = recorder })
	m := initialModel(Options{})
	m.api = newUndoClient(NewClient(env.URL, env.HTTP))
	m.width = 100
	m.scope.inbox = true
	_, cmd := m.Update(nil)
	runCmd(cmd)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	require.Len(t, recorder.captured, 1)
	assert.Subset(t, recorder.captured[0], map[string]any{"screen": "inbox", "surface": "tui"})
}

func TestScreenViewsFocusRolloverAndDaemonSwitch(t *testing.T) {
	var screens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ui/telemetry" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Event      string            `json:"event"`
			Properties map[string]string `json:"properties"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if body.Event == "screen_viewed" {
			screens = append(screens, body.Properties["screen"])
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	m := initialModel(Options{})
	m.api = newUndoClient(NewClient(srv.URL, srv.Client()))
	require.True(t, m.View().ReportFocus)
	m.width = 70
	next, cmd := m.Update(nil)
	m = next.(Model)
	runCmd(cmd)
	require.Empty(t, screens, "the too-narrow hint hides the screen")
	m.width = 120
	m.layout = splitlayout.Split
	m.focus = focusList
	next, cmd = m.Update(nil)
	m = next.(Model)
	runCmd(cmd)
	m.focus = focusDetail
	next, cmd = m.Update(nil)
	m = next.(Model)
	runCmd(cmd)
	require.Equal(t, []string{"issues", "issue"}, screens)
	m.telemetryDay = "2020-01-01"
	next, cmd = m.Update(nil)
	m = next.(Model)
	runCmd(cmd)
	require.Len(t, screens, 2, "background work is not a later-day visit")
	next, cmd = m.Update(tea.FocusMsg{})
	m = next.(Model)
	runCmd(cmd)
	require.Equal(t, []string{"issues", "issue", "issue"}, screens)
	next, cmd = m.Update(daemonSwitchResultMsg{conn: daemonConnection{api: NewClient(srv.URL, srv.Client()), init: bootInit{view: viewEmpty}}})
	m = next.(Model)
	runCmd(cmd)
	require.Equal(t, "empty", screens[len(screens)-1])
	require.Equal(t, uint64(1), m.connGen)
}

func TestScreenName(t *testing.T) {
	m := initialModel(Options{})
	for _, test := range []struct {
		width  int
		view   viewID
		inbox  bool
		screen string
	}{
		{100, viewList, true, "inbox"}, {100, viewList, false, "issues"}, {100, viewDetail, false, "issue"},
		{100, viewHelp, false, "help"}, {100, viewEmpty, false, "empty"}, {100, viewProjects, false, "projects"},
		{100, viewDaemons, false, "daemons"}, {100, viewFederation, false, "federation"}, {100, viewCredentials, false, "credentials"},
		{70, viewList, false, ""}, {70, viewDetail, false, ""}, {70, viewHelp, false, ""}, {70, viewEmpty, false, ""}, {70, viewDaemons, false, ""},
		{70, viewProjects, false, "projects"}, {70, viewFederation, false, "federation"}, {70, viewCredentials, false, "credentials"},
	} {
		m.width, m.height = test.width, 30
		m.view = test.view
		m.scope.inbox = test.inbox
		require.Equal(t, test.screen, m.screenName())
	}
}

func (*recordingTelemetry) SanitizeProperties(event string, properties map[string]any) (map[string]any, error) {
	reporter, err := telemetry.NewReporter(telemetry.Options{})
	if err != nil {
		return nil, err
	}
	return reporter.SanitizeProperties(event, properties)
}
