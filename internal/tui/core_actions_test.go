package tui

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoreActionsReportChangedMutations(t *testing.T) {
	for _, tc := range []struct {
		name            string
		changed         bool
		status          int
		telemetryStatus int
	}{
		{"changed", true, 200, 202}, {"no-op", false, 200, 202},
		{"failed mutation", true, 500, 202}, {"failed telemetry", true, 200, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				var events []map[string]any
				hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/api/v1/ui/telemetry" {
						var event map[string]any
						require.NoError(t, json.NewDecoder(req.Body).Decode(&event))
						mu.Lock()
						events = append(events, event)
						mu.Unlock()
						response := jsonResponse(t, map[string]any{"status": "queued"})
						response.StatusCode = tc.telemetryStatus
						return response, nil
					}
					require.Equal(t, http.MethodPost, req.Method)
					response := jsonResponse(t, map[string]any{"changed": tc.changed})
					response.StatusCode = tc.status
					return response, nil
				})}
				c := NewClient("http://127.0.0.1:7777", hc)
				_, err := c.CreateIssue(t.Context(), 7, CreateIssueBody{Title: "Example task", Actor: "user-a"})
				assert.Equal(t, tc.status != 200, err != nil)
				synctest.Wait()
				_, err = c.Close(t.Context(), 7, "abcd", "user-a")
				assert.Equal(t, tc.status != 200, err != nil)
				synctest.Wait()
				_, err = c.CloseWithEvidence(t.Context(), 7, "abcd", CloseInput{Actor: "user-a", Reason: "done"})
				assert.Equal(t, tc.status != 200, err != nil)
				synctest.Wait()
				_, err = c.Reopen(t.Context(), 7, "abcd", "user-a")
				assert.Equal(t, tc.status != 200, err != nil)
				synctest.Wait()
				if tc.changed && tc.status == 200 {
					assert.Equal(t, []map[string]any{
						{"event": "issue_created", "properties": map[string]any{"surface": "tui"}},
						{"event": "issue_closed", "properties": map[string]any{"surface": "tui"}},
						{"event": "issue_closed", "properties": map[string]any{"surface": "tui"}},
					}, events)
				} else {
					assert.Empty(t, events)
				}
			})
		})
	}
}

func TestUndoReopenCloseDoesNotReportIssueClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var events []string
		hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/v1/instance":
				return jsonResponse(t, map[string]any{"instance_uid": "instance-1"}), nil
			case "/api/v1/projects/7/issues/issue-1":
				return jsonResponse(t, map[string]any{"issue": map[string]any{
					"uid": "issue-1", "project_id": 7, "status": "open", "revision": 5,
				}}), nil
			case "/api/v1/projects/7/issues/issue-1/actions/close":
				return jsonResponse(t, map[string]any{
					"issue":   map[string]any{"uid": "issue-1", "project_id": 7, "status": "closed", "revision": 6},
					"changed": true,
				}), nil
			case "/api/v1/ui/telemetry":
				var event struct {
					Event string `json:"event"`
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&event))
				events = append(events, event.Event)
				response := jsonResponse(t, map[string]any{"status": "queued"})
				response.StatusCode = http.StatusAccepted
				return response, nil
			default:
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
				return nil, nil
			}
		})}
		client := NewClient("http://127.0.0.1:7777", hc)
		undo := newUndoClient(client)
		done := "done"
		entry := undoEntry{
			kind: "reopen", uid: "issue-1", projectID: 7, instanceUID: "instance-1", revision: 5,
			actor: "user-a", before: Issue{Status: "closed", ClosedReason: &done, Revision: 4},
			after: Issue{Status: "open", Revision: 5},
		}
		outcome := undo.undo(t.Context(), entry, false, nil)
		require.NoError(t, outcome.err)
		require.True(t, outcome.changed)
		outcome.attempt.complete()
		synctest.Wait()
		assert.Empty(t, events, "undoing a reopen is not a user close action")

		outcome = undo.undo(t.Context(), entry, true, &CloseInput{
			Actor: "user-a", Reason: "done", Message: "Completed the requested work and checked the result.",
		})
		require.NoError(t, outcome.err)
		require.True(t, outcome.changed)
		outcome.attempt.complete()
		synctest.Wait()
		assert.Empty(t, events, "undoing a reopen is not a user close action")
		assert.Empty(t, events, "undoing a reopen is not a user close action")
	})
}

func TestReportSessionEndedWaitsForIssueAction(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	for _, tc := range []struct {
		name     string
		delay    time.Duration
		switched bool
	}{
		{"accepted", 500 * time.Millisecond, false},
		{"timed out", time.Second, false},
		{"switched daemon accepted", 500 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := make(chan struct{})
				finished := make(chan struct{})
				var mu sync.Mutex
				var completed []string
				hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path != "/api/v1/ui/telemetry" {
						return jsonResponse(t, map[string]any{"changed": true}), nil
					}
					var event struct {
						Event string `json:"event"`
					}
					require.NoError(t, json.NewDecoder(req.Body).Decode(&event))
					if event.Event == "issue_closed" {
						defer close(finished)
						close(started)
						if tc.delay == time.Second {
							<-req.Context().Done()
						} else {
							time.Sleep(tc.delay)
						}
					}
					mu.Lock()
					completed = append(completed, req.URL.Host+"/"+event.Event)
					mu.Unlock()
					if req.Context().Err() != nil {
						return nil, req.Context().Err()
					}
					response := jsonResponse(t, map[string]any{"status": "queued"})
					response.StatusCode = http.StatusAccepted
					return response, nil
				})}
				c := NewClient("http://127.0.0.1:7777", hc)
				active := c
				if tc.switched {
					m := buildRunModel(Options{}, c, bootInit{view: viewEmpty})
					active = NewClient("http://127.0.0.1:8888", hc)
					_, _ = m.installDaemonConnection(daemonConnection{api: active, init: bootInit{view: viewEmpty}})
				}
				before := time.Now()
				resp, err := active.Close(t.Context(), 7, "abcd", "user-a")
				require.NoError(t, err)
				require.True(t, resp.Changed)
				assert.Equal(t, time.Duration(0), time.Since(before))
				<-started
				require.NoError(t, c.ReportSessionEnded(time.Minute))
				assert.Equal(t, tc.delay, time.Since(before))
				mu.Lock()
				actionHost := "127.0.0.1:7777"
				if tc.switched {
					actionHost = "127.0.0.1:8888"
				}
				assert.Equal(t, []string{actionHost + "/issue_closed", "127.0.0.1:7777/session_ended"}, completed)
				mu.Unlock()
				<-finished
				synctest.Wait()
			})
		})
	}
}

func TestSessionEndDoesNotStartLateActionReports(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mutationStarted := make(chan struct{})
		acknowledge := make(chan struct{})
		var events []string
		hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/api/v1/ui/telemetry" {
				close(mutationStarted)
				<-acknowledge
				return jsonResponse(t, map[string]any{"changed": true}), nil
			}
			var event struct {
				Event string `json:"event"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&event))
			events = append(events, event.Event)
			response := jsonResponse(t, map[string]any{"status": "queued"})
			response.StatusCode = http.StatusAccepted
			return response, nil
		})}
		c := NewClient("http://127.0.0.1:7777", hc)
		closed := make(chan bool)
		go func() {
			resp, err := c.Close(t.Context(), 7, "abcd", "user-a")
			closed <- err == nil && resp != nil && resp.Changed
		}()
		<-mutationStarted
		require.NoError(t, c.ReportSessionEnded(time.Minute))
		close(acknowledge)
		require.True(t, <-closed)
		synctest.Wait()
		assert.Equal(t, []string{"session_ended"}, events)
	})
}
