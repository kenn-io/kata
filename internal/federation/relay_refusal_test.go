package federation

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

const relayRefusalBody = `{"error":{"code":"relay_cross_project_links","message":"this daemon's copy of the project has cross-project issue links","hint":"private remedy","data":{"secret":"private diagnostic"}}}`

func TestRelayResetRefusalAttribution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		canonical bool
	}{
		{"refusal", 409, relayRefusalBody, true},
		{"other status", 503, relayRefusalBody, false},
		{"other code", 409, `{"error":{"code":"relay_conflict","message":"other conflict"}}`, false},
		{"malformed", 409, `{"error":{"code":"relay_cross_project_links"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{baseURL: "https://hub.example", client: &http.Client{Transport: pushOnlyTestTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}}
			_, err := client.RelayReset(t.Context(), 42)
			var status *HubStatusError
			require.ErrorAs(t, err, &status)
			require.Equal(t, tc.status, status.HTTPStatus())
			require.Equal(t, tc.canonical, errors.Is(err, errRelayCrossProjectLinks))
			if tc.canonical {
				canonical, marshalErr := json.Marshal(api.NewRelayCrossProjectLinksError("hub").Envelope())
				require.NoError(t, marshalErr)
				require.JSONEq(t, string(canonical), status.Body)
			} else {
				require.Equal(t, tc.body, status.Body)
			}
		})
	}
}

// The checked contract measures delay from completion, honors long configured
// intervals, and forgets a refusal after success or a different failure.
func TestRelayRefusalRetryLifecycle(t *testing.T) {
	for _, tc := range []struct {
		interval time.Duration
		delays   []time.Duration
	}{
		{30 * time.Second, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}},
		{4 * time.Minute, []time.Duration{4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}},
		{10 * time.Minute, []time.Duration{10 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}},
	} {
		t.Run(tc.interval.String(), func(t *testing.T) {
			binding := db.FederationBinding{ProjectID: 1}
			completed := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
			refused := fmt.Errorf("sync failed: %w", &HubStatusError{StatusCode: 409, relayCrossProjectLinks: true})
			states := make(relayRefusalRetries)
			for _, delay := range tc.delays {
				states.record(binding, refused, tc.interval, completed)
				require.True(t, states.blocked(binding, completed.Add(delay-time.Nanosecond)))
				require.False(t, states.blocked(binding, completed.Add(delay)))
				completed = completed.Add(delay + 10*time.Second)
			}
			for _, err := range []error{nil, errors.New("different failure")} {
				states.record(binding, err, tc.interval, completed)
				require.False(t, states.blocked(binding, completed))
				states.record(binding, refused, tc.interval, completed)
				require.False(t, states.blocked(binding, completed.Add(tc.delays[0])), "a fresh refusal starts at the first delay")
			}
		})
	}
}
