package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestCronSpokeForwardRefusesProjectArchivedDuringPreflight(t *testing.T) {
	f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
		cfg.Auth = config.AuthConfig{Token: "owner-token"}
		push := cfg.FederationPushPending
		cfg.FederationPushPending = func(ctx context.Context, b db.FederationBinding, c config.FederationCredential) error {
			if err := push(ctx, b, c); err != nil {
				return err
			}
			project, _, err := f.spoke.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.sp.ID, Actor: "tester", Force: true})
			require.NoError(t, err)
			require.NotNil(t, project.DeletedAt)
			return nil
		}
	})
	response, raw := envDoRaw(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", f.sp.ID, cronHTTPUID(t)), map[string]any{"actor": "tester", "name": "Review", "definition": f.definition}, map[string]string{"Authorization": "Bearer owner-token"})
	jobs, err := f.hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: f.hp.ID})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode, "%s", raw)
	require.Contains(t, string(raw), `"code":"project_not_found"`)
	require.Zero(t, f.writes.Load(), "archived projects must refuse before sending a hub job mutation")
	require.Empty(t, jobs)
}
