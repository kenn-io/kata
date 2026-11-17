package tui

import (
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const spokeNameTestUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"

func TestClientSetFederationSpokeName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spokeName  string
		wantMethod string
		wantBody   map[string]any
	}{
		{name: "set", spokeName: "build-box", wantMethod: http.MethodPut, wantBody: map[string]any{"name": "build-box"}},
		{name: "empty clears", spokeName: "", wantMethod: http.MethodDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod string
			var gotBody map[string]any
			var writes atomic.Int32
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					respondJSON(t, w, map[string]any{"ok": true, "api_schema_version": "0.54.0"})
				case "/api/v1/federation/enrollments/spokes/" + spokeNameTestUID + "/name":
					writes.Add(1)
					gotMethod = r.Method
					if r.ContentLength > 0 {
						require.NoError(t, json.UnmarshalRead(r.Body, &gotBody))
					}
					respondJSON(t, w, map[string]any{"spoke_instance_uid": spokeNameTestUID, "spoke_name": tc.spokeName})
				default:
					http.NotFound(w, r)
				}
			})

			require.NoError(t, client.SetFederationSpokeName(t.Context(), spokeNameTestUID, tc.spokeName))

			assert.Equal(t, int32(1), writes.Load())
			assert.Equal(t, tc.wantMethod, gotMethod)
			assert.Equal(t, tc.wantBody, gotBody)
		})
	}
}

// A daemon from before the name routes would answer 404 or ignore the body;
// the TUI refuses it before sending, and the failure is a preflight one so the
// caller knows nothing was written.
func TestClientSetFederationSpokeNameRefusesOlderDaemonBeforeWriting(t *testing.T) {
	var writes atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			respondJSON(t, w, map[string]any{"ok": true, "api_schema_version": "0.37.0"})
		default:
			writes.Add(1)
			http.NotFound(w, r)
		}
	})

	err := client.SetFederationSpokeName(t.Context(), spokeNameTestUID, "build-box")

	require.ErrorContains(t, err, "naming a spoke requires daemon API 0.38.0 or newer; upgrade the daemon")
	var preflight *mutationPreflightError
	require.ErrorAs(t, err, &preflight)
	assert.Zero(t, writes.Load())
}
