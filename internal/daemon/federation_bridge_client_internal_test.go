package daemon

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
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
		{"oversized relay conflict", 409, fmt.Sprintf(`{"error":{"code":"relay_cross_project_links","message":"%s"}}`, strings.Repeat("x", (1<<20)+1)), "hub_request_rejected", 502},
		{"relay private diagnostics", 409, `{"error":{"code":"relay_cross_project_links","message":"private diagnostic","hint":"private remedy","data":{"token":"private-token"}}}`, "relay_cross_project_links", 409},
		{"untrusted conflict", 409, `{"error":{"code":"private_diagnostic","message":"secret"}}`, "hub_request_rejected", 502},
		{"redirect", 302, `{}`, "hub_request_rejected", 502},
		{"unavailable", 503, `{}`, "hub_request_rejected", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := strings.NewReader(test.body)
			body := &bridgeTrackedResponseBody{Reader: reader}
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
				if test.code == "relay_cross_project_links" {
					canonical := api.NewRelayCrossProjectLinksError("hub")
					canonical.Data = map[string]any{"hub_status": test.status}
					require.Equal(t, canonical, failure)
				}
				if test.status != 200 {
					require.Equal(t, test.status, failure.Data["hub_status"], "callers decide from the hub's own status")
				}
			}
			require.LessOrEqual(t, len(test.body)-reader.Len(), (1<<20)+1, "response reads must stop after the size limit plus one detection byte")
			require.Equal(t, 1, transport.attempts)
			require.True(t, body.closed, "the original response body must close after bounded capture")
		})
	}
}

func TestRelayCrossProjectLinksErrorMapping(t *testing.T) {
	hub := api.NewRelayCrossProjectLinksError("hub")
	require.Contains(t, hub.Message, "the project on the hub has cross-project issue links")
	require.Contains(t, hub.Hint, "remove cross-project related, blocks, and parent links on the hub")
	require.Contains(t, hub.Hint, "ask an operator")
	require.NotContains(t, hub.Message, "this daemon's copy")
	require.NotContains(t, hub.Message, "private")
	require.NotContains(t, hub.Hint, "private")
	require.Nil(t, hub.Data)

	err := errors.Join(db.ErrFederatedReadOnly, db.ErrFederationCrossProjectBoundary)
	mapped := federationReplicaAPIError(err)
	failure, ok := errors.AsType[*api.APIError](mapped)
	require.True(t, ok)
	require.Equal(t, http.StatusConflict, failure.Status)
	require.Equal(t, "relay_cross_project_links", failure.Code)
	require.Contains(t, failure.Hint, "remove")
	require.NotContains(t, failure.Hint, "on the hub", "local relay setup may be the source of this error")
}
