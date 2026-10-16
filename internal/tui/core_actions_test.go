package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

func TestCoreActionResultDoesNotWaitForTelemetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		delivered := make(chan struct{})
		hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/api/v1/ui/telemetry" {
				<-req.Context().Done()
				close(delivered)
				return nil, req.Context().Err()
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"changed":true}`))}, nil
		})}
		c := NewClient("http://127.0.0.1:7777", hc)
		started := time.Now()
		resp, err := c.Close(t.Context(), 7, "abcd", "user-a")
		require.NoError(t, err)
		require.True(t, resp.Changed)
		assert.Equal(t, time.Duration(0), time.Since(started))
		<-delivered
		synctest.Wait()
		assert.Equal(t, time.Second, time.Since(started))
	})
}
