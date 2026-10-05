package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R10: negotiate relay, signed provenance, and portable artifacts independently
// of the storage schema before the bridge enrollment flow creates state.
func TestInstanceAdvertisesSeparateRelayCapabilities(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		code, _, body := f.request(t, http.MethodGet, "/api/v1/instance", "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		var record map[string]any
		require.NoError(t, json.Unmarshal(body, &record))
		for _, name := range []string{"relay_protocol_version", "provenance_protocol_version", "embedding_artifact_protocol_version"} {
			t.Run(name, func(t *testing.T) { require.Equal(t, float64(1), record[name]) })
		}
		require.NotContains(t, string(body), f.private.UID)
		require.NotContains(t, string(body), projectAccessCanary)
	})
}
