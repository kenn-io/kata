package daemon_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R6/R10: the upstream operator diagnostic contract survives project policy.
func TestProjectAccessDoctorOperatorAuthority(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		sink := &doctorSink{}
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Hooks: sink, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		for _, actor := range []string{"member", "nonmember"} {
			status, _, body := f.request(t, http.MethodGet, "/api/v1/doctor", actor, nil, nil)
			require.Equal(t, http.StatusForbidden, status, string(body))
			require.NotContains(t, string(body), projectAccessCanary)
			require.Zero(t, sink.reads.Load(), "diagnostics must not be collected for ordinary project principals")
		}
		status, _, body := f.request(t, http.MethodGet, "/api/v1/doctor", "admin", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		require.EqualValues(t, 1, sink.reads.Load())
	})
}
