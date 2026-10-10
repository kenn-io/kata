package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCapabilitiesEmptyProjectReadOnly(t *testing.T) {
	env := testenv.New(t, testenv.WithInsecureReadonly())
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	before, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	response, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil, nil)
	require.Equalf(t, 200, response.StatusCode, "%s", raw)
	require.Equal(t, "cron_v1", response.Header.Get(db.EventFeaturesHeader))
	require.Contains(t, string(raw), project.UID)
	after, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

type capabilitiesReadHost struct {
	projectID int64
	calls     []daemon.HostOperation
}

func (h *capabilitiesReadHost) Authorize(_ context.Context, r daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	h.calls = append(h.calls, r.Operation)
	if r.Operation.Policy.Mutation || len(r.Operation.ProjectIDs) != 1 || r.Operation.ProjectIDs[0] != h.projectID {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostAccessDecision{}, nil
}
func TestCronCapabilitiesProjectScopedHostRead(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	host := &capabilitiesReadHost{projectID: project.ID}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, tc := range []struct {
		id   int64
		want int
	}{{project.ID, 200}, {other.ID, 404}} {
		request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", tc.id), nil)
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		require.Equalf(t, tc.want, recorder.Code, "%s", recorder.Body.String())
	}
	require.Len(t, host.calls, 2)
	require.Equal(t, "getCronCapabilities", host.calls[0].ID)
	require.False(t, host.calls[0].Policy.Mutation)
}
