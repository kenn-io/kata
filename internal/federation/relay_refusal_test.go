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
		{"truncated", 409, `{"error":{"code":"relay_cross_project_links","message":"` + strings.Repeat("x", 5000) + `"}}`, false},
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
				require.Equal(t, tc.body[:min(len(tc.body), 4096)], status.Body)
			}
		})
	}
}

// The checked contract measures delay from completion, honors long configured
// intervals, and forgets a refusal after success or a change of binding identity.
func TestRelayRefusalRetryLifecycle(t *testing.T) {
	for _, interval := range []time.Duration{30 * time.Second, 4 * time.Minute, 10 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			binding := db.FederationBinding{ProjectID: 1, HubURL: "https://hub.example", HubProjectID: 42, RelayConfig: &db.RelayBindingConfig{BindingUID: "binding", UpstreamInstanceUID: "upstream", ResetEpoch: 1}}
			completed := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
			refused := fmt.Errorf("sync failed: %w", &HubStatusError{StatusCode: 409, relayCrossProjectLinks: true})
			states := make(relayRefusalRetries)
			for i := range 6 {
				states.record(binding, refused, interval, completed)
				delay := max(time.Minute, interval)
				for range i {
					delay = max(interval, min(2*delay, 5*time.Minute))
				}
				require.True(t, states.blocked(binding, completed.Add(delay-time.Nanosecond)))
				require.False(t, states.blocked(binding, completed.Add(delay)))
				completed = completed.Add(delay + 10*time.Second)
			}
			for _, err := range []error{nil, errors.New("different failure")} {
				states.record(binding, err, interval, completed)
				require.False(t, states.blocked(binding, completed))
				states.record(binding, refused, interval, completed)
				require.False(t, states.blocked(binding, completed.Add(max(time.Minute, interval))), "a fresh refusal starts at the first delay")
			}
			for _, change := range []func(*db.FederationBinding){
				func(b *db.FederationBinding) { b.HubURL = "https://other.example" },
				func(b *db.FederationBinding) { b.CreatedAt = completed },
				func(b *db.FederationBinding) { b.RelayConfig.BindingUID = "replacement" },
				func(b *db.FederationBinding) { b.RelayConfig.UpstreamInstanceUID = "replacement" },
				func(b *db.FederationBinding) { b.RelayConfig.ResetEpoch++ },
			} {
				changed := binding
				c := *binding.RelayConfig
				changed.RelayConfig = &c
				change(&changed)
				states.record(binding, refused, interval, completed)
				states.reconcile([]activeSpokeBinding{{binding: changed}})
				require.Empty(t, states)
			}
			states.record(binding, refused, interval, completed)
			states.reconcile(nil) // Includes disabled, archived, and removed bindings.
			require.Empty(t, states)
		})
	}
}
