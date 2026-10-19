package tui

import (
	"context"
	"encoding/json"
 "fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
)

type recordingTelemetry struct {
	mu       sync.Mutex
	captured []map[string]any
	opened   chan struct{}
	ended    chan map[string]any
	release  <-chan struct{}
	finished chan struct{}
}

func (*recordingTelemetry) EventAllowed(event string) bool {
	return event == "app_opened" || event == "session_ended" || event == "screen_viewed"
}
func (*recordingTelemetry) Enabled() bool { return true }
func (r *recordingTelemetry) Capture(event string, properties map[string]any) error {
	if event == "app_opened" && r.opened != nil {
		close(r.opened)
	}
	if event == "session_ended" && r.ended != nil {
		r.ended <- properties
		<-r.release
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captured = append(r.captured, properties)
	if event == "session_ended" && r.finished != nil {
		close(r.finished)
	}
	return nil
}

func TestReportAppOpenedReachesDaemonCapture(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	recorder := &recordingTelemetry{finished: make(chan struct{})}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = recorder })

	c := NewClient(env.URL, env.HTTP)
	require.NoError(t, c.ReportAppOpened(t.Context()))
	c.ReportSessionEnded(2 * time.Minute)
	select {
	case <-recorder.finished:
	case <-t.Context().Done():
		t.Fatal("session_ended delivery did not finish")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	assert.Equal(t, []map[string]any{{"surface": "tui"}, {"surface": "tui", "duration_bucket": "1_to_5m"}}, recorder.captured)
}

func TestReportSessionEndedBoundsDelivery(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
		c := NewClient("http://127.0.0.1:7777", hc)
		started := time.Now()
		c.ReportSessionEnded(time.Minute)
		assert.Equal(t, 50*time.Millisecond, time.Since(started))
		time.Sleep(time.Second)
		synctest.Wait()
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
	t.Setenv("KATA_HOME", t.TempDir())
	var reports atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	switched := httptest.NewServer(handler)
	t.Cleanup(switched.Close)
	init := bootInit{scope: scope{projectID: 7, projectName: "example-project"}, view: viewList}
	m := buildRunModel(Options{}, NewClient(srv.URL, srv.Client()), init)

	batch, ok := m.Init()().(tea.BatchMsg)
	require.True(t, ok)
	for _, cmd := range batch[1:] { // batch[0] waits on the SSE channel
		runCmd(cmd)
	}
	assert.Equal(t, int32(1), reports.Load())

	m, cmd := m.installDaemonConnection(daemonConnection{api: NewClient(switched.URL, switched.Client()), init: init})
	runCmd(cmd)
	m, cmd = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: m.connGen})
	runCmd(cmd)
	assert.Equal(t, int32(1), reports.Load(), "daemon switches and capability refetches must not report again")
}

type noAppOpenedAPI struct{ KataAPI }

func TestReportAppOpenedFailureIsSilent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
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
			transport := test.client.hc.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			var requests atomic.Int32
			hc := *test.client.hc
			hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/ui/telemetry" {
					requests.Add(1)
				}
				return transport.RoundTrip(r)
			})
			m := buildRunModel(Options{}, NewClient(test.client.base, &hc), bootInit{})

			cmd := m.reportAppOpened()
			require.NotNil(t, cmd)
			assert.Nil(t, cmd())
			assert.Equal(t, int32(1), requests.Load())
			assert.Nil(t, m.toast)
		})
	}

	m := initialModel(Options{})
	m.api = noAppOpenedAPI{}
	assert.Nil(t, m.reportAppOpened())
}

func TestScreenViewsThroughModelAndDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	recorder := &recordingTelemetry{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = recorder })
	m := buildRunModel(Options{}, NewClient(env.URL, env.HTTP), bootInit{view: viewList})
	m.width = 100
	m.scope.inbox = true
	_, cmd := m.Update(nil)
	require.NoDirExists(t, filepath.Join(home, "telemetry"), "Update leaves marker work to its command")
	recorder.mu.Lock()
	assert.Empty(t, recorder.captured)
	recorder.mu.Unlock()
	runCmd(cmd)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	require.Len(t, recorder.captured, 1)
	assert.Subset(t, recorder.captured[0], map[string]any{"screen": "inbox", "surface": "tui"})
}

func TestScreenViewsLayoutAndDaemonSwitch(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
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
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer srv.Close()
	m := buildRunModel(Options{}, NewClient(srv.URL, srv.Client()), bootInit{view: viewList})
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
	next, cmd = m.Update(daemonSwitchResultMsg{conn: daemonConnection{api: NewClient(srv.URL, srv.Client()), init: bootInit{view: viewEmpty}}})
	m = next.(Model)
	runCmd(cmd)
	require.Equal(t, "empty", screens[len(screens)-1])
	require.Equal(t, uint64(1), m.connGen)
	t.Run("model visit rollover", func(t *testing.T) {
		t.Setenv("KATA_HOME", t.TempDir())
		synctest.Test(t, func(t *testing.T) {
			r := &screenVisitRecorder{}
			m := initialModel(Options{})
			m.api, m.width, m.view = r, 100, viewList
			runCmd(m.screenViewCommand(false))
			time.Sleep(24 * time.Hour)
			runCmd(m.screenViewCommand(false))
			require.Equal(t, []string{"issues"}, r.screens)
			runCmd(m.screenViewCommand(true))
			require.Equal(t, []string{"issues", "issues"}, r.screens)
		})
	})
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

type screenVisitRecorder struct {
	KataAPI
	screens []string
}

func (r *screenVisitRecorder) ReportScreenViewed(_ context.Context, screen string) error {
	r.screens = append(r.screens, screen)
	return nil
}

func (r *recordingTelemetry) SanitizeProperties(event string, properties map[string]any) (map[string]any, error) {
	if !r.EventAllowed(event) {
		return nil, telemetry.ErrUnsupportedEvent
	}
	return properties, nil
}

// Accepted daily claims survive a restart on the same daemon.
func TestTelemetryDailyScreens(t *testing.T) {
	for _, event := range []string{"screen_viewed", "app_opened"} {
		t.Run(event, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				var requests int
				hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Header: make(http.Header), Request: r}, nil
				})}
				report := func(c *Client, screen string) {
					if event == "app_opened" {
						require.NoError(t, c.ReportAppOpened(t.Context()))
					} else {
						require.NoError(t, c.ReportScreenViewed(t.Context(), screen))
					}
				}
				c := NewClient("http://daemon.example", hc)
				report(c, "issues")
				if event == "screen_viewed" {
					report(c, "help")
				}
				report(NewClient(c.base, hc), "issues")
				if event == "screen_viewed" {
					require.Equal(t, 2, requests, "Issues, Help, restart, Issues reports two screens")
				} else {
					require.Equal(t, 1, requests)
				}
				time.Sleep(24 * time.Hour)
				report(c, "issues")
				report(NewClient(c.base, hc), "issues")
				if event == "screen_viewed" {
					require.Equal(t, 3, requests)
				} else {
					require.Equal(t, 2, requests)
				}
			})
		})
	}
}

func TestTelemetryDisabledSerializesConcurrentReports(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		first, release := make(chan struct{}), make(chan struct{})
		c := NewClient("http://daemon.example", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if requests.Add(1) == 1 {
				close(first)
			}
			<-release
			return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"status":"disabled"}`)), Header: make(http.Header), Request: r}, nil
		})})
		var wg sync.WaitGroup
		wg.Go(func() { assert.NoError(t, c.ReportAppOpened(t.Context())) })
		<-first
		wg.Go(func() { assert.NoError(t, c.ReportScreenViewed(t.Context(), "issues")) })
		synctest.Wait()
		assert.Equal(t, int32(1), requests.Load())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.ErrorIs(t, c.ReportScreenViewed(ctx, "issues"), context.Canceled)
		close(release)
		wg.Wait()
		synctest.Wait()
		require.Equal(t, int32(1), requests.Load())
		time.Sleep(24 * time.Hour)
		require.NoError(t, c.ReportScreenViewed(t.Context(), "help"))
		require.Equal(t, int32(2), requests.Load())
	})
}

func TestTelemetryGateAcrossDaemonSwitches(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	requests := map[string]int{}
	client := func(target string) *Client {
		return NewClient("http://"+target+".example", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests[target]++
			responseStatus := "disabled"
			if target == "b" {
				responseStatus = "queued"
			}
			return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"status":"` + responseStatus + `"}`)), Header: make(http.Header), Request: r}, nil
		})})
	}
	a := client("a")
	m := buildRunModel(Options{}, a, bootInit{})
	require.NoError(t, a.ReportScreenViewed(t.Context(), "issues"))
	b := client("b")
	m, _ = m.installDaemonConnection(daemonConnection{api: b})
	require.NoError(t, b.ReportScreenViewed(t.Context(), "issues"))
	// Another terminal caller can receive an opt-out after this gate's first check.
	clientpkg.RecordTelemetryDayMarker(clientpkg.TelemetryDisabledMarkerPath(b.telemetryTarget), time.Now().UTC().Format(time.DateOnly))
	require.NoError(t, b.reportUsage(t.Context(), "session_ended", map[string]any{"surface": "tui", "duration_bucket": "1_to_5m"}))
	require.NoError(t, os.Remove(clientpkg.TelemetryDisabledMarkerPath(a.telemetryTarget)))
	again := client("a")
	_, _ = m.installDaemonConnection(daemonConnection{api: again})
	require.NoError(t, again.reportUsage(t.Context(), "session_ended", map[string]any{"surface": "tui", "duration_bucket": "1_to_5m"}))
	require.Equal(t, map[string]int{"a": 1, "b": 1}, requests)
}

func TestTelemetryRejectsInvalidReceipts(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	for _, receipt := range []string{`{}`, `{"status":"unexpected"}`} {
		t.Run(receipt, func(t *testing.T) {
			c := NewClient("http://daemon.example", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(receipt)), Header: make(http.Header), Request: r}, nil
			})})
			require.Error(t, c.ReportScreenViewed(t.Context(), "issues"))
		})
	}
}

func TestOutcomesSubmittedEmptySearch(t *testing.T) {
	recorder := &recordingTelemetry{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = recorder })
	for _, tc := range []struct {
		name, query              string
		match, form, fresh, down bool
		pendingScope             string
		want                     int
	}{
		{name: "empty", query: "missing", want: 1},
		{name: "empty from results", query: "missing", down: true, want: 1},
		{name: "match", query: "Example", match: true},
		{name: "blank", query: " "},
		{name: "unchanged filter form", query: "missing", form: true},
		{name: "changed filter form", query: "missing", form: true, fresh: true, want: 1},
		{name: "same scope", query: "missing", pendingScope: "same scope", want: 1},
		{name: "daemon", query: "missing", pendingScope: "daemon", want: 1},
		{name: "project", query: "missing", pendingScope: "project", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder.captured = nil
			recorder.events = nil
			m := mFixtureForBar()
			m.api = newUndoClient(NewClient(env.URL, env.HTTP))
			m.list.loading = tc.pendingScope != ""
			if tc.match {
				m.list.issues = []Issue{{UID: "example-issue", Title: "Example"}}
			}
			m = openBarFromCmd(t, m, '/')
			var cmd tea.Cmd
			for _, r := range tc.query {
				m, cmd = stepModel(m, runeKey(r))
				runCmd(cmd)
			}
			require.Empty(t, recorder.captured, "typing is only a preview")
			if tc.form {
				if tc.fresh {
					m.list.filter.Search = ""
				}
				m, cmd = m.commitFilterForm(newFilterForm(ListFilter{Search: tc.query}, scope{}))
			} else {
				if tc.down {
					m, cmd = stepModel(m, tea.KeyPressMsg{Code: tea.KeyDown})
					runCmd(cmd)
				}
				m, cmd = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			}
			runCmd(cmd)
			if tc.pendingScope != "" {
				require.Empty(t, recorder.captured)
				m, cmd = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				runCmd(cmd)
				switch tc.pendingScope {
				case "daemon":
					m, _ = m.installDaemonConnection(daemonConnection{
						api:  NewClient(env.URL, env.HTTP),
						init: bootInit{scope: m.scope, view: viewList},
					})
				case "project":
					m.projectsCursor = 0
					m, _ = m.applyProjectsViewSelection([]projectsRow{{projectID: 99, name: "example-project"}})
				}
				if tc.pendingScope != "same scope" {
					m.list.loading = true
					m = openBarFromCmd(t, m, '/')
					for _, r := range tc.query {
						m, cmd = stepModel(m, runeKey(r))
						runCmd(cmd)
					}
					require.Empty(t, recorder.captured, "typing is only a preview")
					m, cmd = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
					runCmd(cmd)
				}
				m, cmd = stepModel(m, initialFetchMsg{dispatchKey: m.currentCacheKey()})
				runCmd(cmd)
			}
			require.Len(t, recorder.captured, tc.want)
			if tc.want > 0 {
				require.Equal(t, []string{"search_empty"}, recorder.events)
				require.Equal(t, map[string]any{"surface": "tui"}, recorder.captured[0])
			}
			if tc.pendingScope != "" {
				_, cmd = stepModel(m, refetchedMsg{dispatchKey: m.currentCacheKey()})
				runCmd(cmd)
				require.Len(t, recorder.captured, 1)
			}
		})
	}
}

func TestOutcomesShownFormErrors(t *testing.T) {
	recorder := &recordingTelemetry{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = recorder })
	for _, tc := range []struct{ origin, kind string }{{"form", "create"}, {"form", "close"}, {"detail", "close"}} {
		t.Run(tc.origin+"/"+tc.kind, func(t *testing.T) {
			recorder.captured, recorder.events = nil, nil
			m := mFixtureForBar()
			m.api = newUndoClient(NewClient(env.URL, env.HTTP))
			m.input = newNewIssueForm()
			if tc.kind == "close" {
				m.input = newCloseForm(formTarget{})
			}
			m.input.formGen = 2
			m.view, m.detail.gen, m.pendingRefetch = viewDetail, 2, true
			next, cmd := m.routeMutation(mutationDoneMsg{origin: tc.origin, kind: tc.kind, formGen: 1, gen: 1, err: fmt.Errorf("refused")})
			runCmd(cmd)
			require.Empty(t, recorder.captured, "discarded response")
			next, cmd = next.(Model).routeMutation(mutationDoneMsg{origin: tc.origin, kind: tc.kind, formGen: 2, gen: 2, err: fmt.Errorf("refused")})
			if tc.origin == "form" {
				require.Contains(t, next.(Model).input.err, "refused")
			} else {
				require.ErrorContains(t, next.(Model).detail.err, "refused")
			}
			runCmd(cmd)
			require.Equal(t, []string{"error_shown"}, recorder.events)
			require.Equal(t, []map[string]any{{"surface": "tui", "kind": map[string]string{"create": "issue_created", "close": "issue_closed"}[tc.kind]}}, recorder.captured)
		})
	}
	for _, kind := range []inputKind{inputNewIssueForm, inputCloseForm} {
		recorder.captured = nil
		recorder.events = nil
		m := mFixtureForBar()
		m.api = newUndoClient(NewClient(env.URL, env.HTTP))
		m.input = newNewIssueForm()
		if kind == inputCloseForm {
			m.input = newCloseForm(formTarget{})
		}
		m, cmd := m.commitFormInput(kind)
		require.NotEmpty(t, m.input.err)
		runCmd(cmd)
		expected := "issue_created"
		if kind == inputCloseForm {
			expected = "issue_closed"
		}
		require.Equal(t, []string{"error_shown"}, recorder.events)
		require.Equal(t, []map[string]any{{"surface": "tui", "kind": expected}}, recorder.captured)
	}
}
