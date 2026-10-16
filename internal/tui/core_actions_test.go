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

func TestReportSessionEndedWaitsForIssueAction(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{"accepted", 500 * time.Millisecond},
		{"timed out", time.Second},
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
						if tc.name == "timed out" {
							<-req.Context().Done()
						} else {
							time.Sleep(tc.delay)
						}
					}
					mu.Lock()
					completed = append(completed, event.Event)
					mu.Unlock()
					if req.Context().Err() != nil {
						return nil, req.Context().Err()
					}
					response := jsonResponse(t, map[string]any{"status": "queued"})
					response.StatusCode = http.StatusAccepted
					return response, nil
				})}
				c := NewClient("http://127.0.0.1:7777", hc)
				before := time.Now()
				resp, err := c.Close(t.Context(), 7, "abcd", "user-a")
				require.NoError(t, err)
				require.True(t, resp.Changed)
				assert.Equal(t, time.Duration(0), time.Since(before))
				<-started
				require.NoError(t, c.ReportSessionEnded(time.Minute))
				assert.Equal(t, tc.delay, time.Since(before))
				mu.Lock()
				assert.Equal(t, []string{"issue_closed", "session_ended"}, completed)
				mu.Unlock()
				<-finished
				synctest.Wait()
			})
		})
	}
}
