package daemon_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestTrustedProxyPreservesIssueScopedBearer(t *testing.T) {
	ts, store, _ := startBearerProxyTestServer(t, "X-Kata-Actor", bearerProxyOpts{
		Token: "bootstrap-token", RequireTokenIdentity: true,
	})
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Root", Author: "coordinator",
	})
	require.NoError(t, err)
	outside, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Outside", Author: "coordinator",
	})
	require.NoError(t, err)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree,
			ProjectUID: project.UID, RootIssueUID: root.UID},
		ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
	})
	require.NoError(t, err)
	base := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/"
	for _, actor := range []string{"", "proxy-admin"} {
		t.Run("actor="+actor, func(t *testing.T) {
			headers := map[string]string{"Authorization": "Bearer worker-token", "X-Kata-Actor": actor}
			resp, raw := doReq(t, ts, http.MethodGet, base+outside.ShortID, nil, headers)
			require.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
			resp, raw = doReq(t, ts, http.MethodPost, base+outside.ShortID+"/comments",
				map[string]string{"actor": "worker", "body": "Forbidden"}, headers)
			require.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
			resp, raw = doReq(t, ts, http.MethodPost, base+root.ShortID+"/comments",
				map[string]string{"actor": "worker", "body": "Allowed"}, headers)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			require.Contains(t, string(raw), `"author":"worker"`)
		})
	}
}
