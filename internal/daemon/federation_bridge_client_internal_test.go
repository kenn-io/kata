package daemon

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

type bridgeTrackedResponseBody struct {
	io.Reader
	closed bool
}

func (b *bridgeTrackedResponseBody) Close() error { b.closed = true; return nil }

type bridgeResponseTransport struct {
	body             *bridgeTrackedResponseBody
	status, attempts int
}

func (t *bridgeResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.attempts++
	return &http.Response{StatusCode: t.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: t.body, Request: request}, nil
}

// The generated bridge client must retain the prior bounded, single-attempt
// transport contract and release the original HTTP response in every case.
func TestFederationBridgeGeneratedTransportBounds(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		body, code  string
		localStatus int
	}{
		{"valid", 200, `{}`, "", 0},
		{"oversized", 200, strings.Repeat("x", (1<<20)+1), "invalid_hub_response", 502},
		{"unauthenticated", 401, `{}`, "hub_credential_rejected", 502},
		{"forbidden", 403, `{}`, "hub_credential_rejected", 502},
		{"missing", 404, `{}`, "hub_request_rejected", 502},
		{"conflict", 409, `{}`, "hub_request_rejected", 502},
		{"redirect", 302, `{}`, "hub_request_rejected", 502},
		{"unavailable", 503, `{}`, "hub_request_rejected", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &bridgeTrackedResponseBody{Reader: strings.NewReader(test.body)}
			transport := &bridgeResponseTransport{body: body, status: test.status}
			client, err := newFederationBridgeAPIClient(&http.Client{Transport: transport}, "https://hub.example")
			require.NoError(t, err)
			response, err := client.InstanceWithResponse(t.Context())
			if test.code == "" {
				require.NoError(t, err)
				require.NotNil(t, response)
			} else {
				failure, ok := errors.AsType[*api.APIError](err)
				require.True(t, ok, "error=%v", err)
				require.Equal(t, test.code, failure.Code)
				require.Equal(t, test.localStatus, failure.Status, "a hub rejection must not look like a local authorization failure")
				if test.status != 200 {
					require.Equal(t, test.status, failure.Data["hub_status"], "callers decide from the hub's own status")
				}
			}
			require.Equal(t, 1, transport.attempts)
			require.True(t, body.closed, "the original response body must close after bounded capture")
		})
	}
}
