package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

func TestHubBusyForwarding(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		requestScope, busy bool
		message            string
	}{
		{"proxy", "Rate exceeded.", true, true, "127.0.0.1 is busy; try again in a minute"},
		{"relay", `{"error":{"code":"server_busy","message":"root.example is busy; try again in a minute"}}`, true, true, "root.example is busy; try again in a minute"},
		{"background", "Rate exceeded.", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(429)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(hub.Close)
			hc := hub.Client()
			observeHubBusy(hc)
			ctx := t.Context()
			if tc.requestScope {
				ctx = context.WithValue(ctx, hubBusyKey{}, true)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, hub.URL, nil)
			require.NoError(t, err)
			resp, err := hc.Do(req) //nolint:gosec // loopback fixture
			if tc.busy {
				var apiErr *api.APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, tc.message, apiErr.Message)
				var headers huma.HeadersError
				require.ErrorAs(t, err, &headers)
				assert.Equal(t, "60", headers.GetHeaders().Get("Retry-After"))
				return
			}
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, errors.Join(err, resp.Body.Close()))
			assert.Equal(t, tc.body, string(body))
		})
	}
}
