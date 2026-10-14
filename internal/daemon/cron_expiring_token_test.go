package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// expiringRunObservationStore waits until the admitted token's expiry has
// passed before the observation transaction runs.
type expiringRunObservationStore struct {
	db.Storage
	expiresAt time.Time
}

func (s expiringRunObservationStore) ObserveCronRun(ctx context.Context, in db.ObserveCronRun) (db.CronRunObservationResult, error) {
	expired := time.NewTimer(time.Until(s.expiresAt) + time.Millisecond)
	defer expired.Stop()
	select {
	case <-expired.C:
	case <-ctx.Done():
		return db.CronRunObservationResult{}, ctx.Err()
	}
	return s.Storage.ObserveCronRun(ctx, in)
}

// An ordinary actor token may carry an expiry. It writes cron state while the
// expiry lies ahead, and the write transaction refuses it once the expiry has
// passed even though the token was admitted before.
func TestCronWriteHonorsOrdinaryTokenExpiry(t *testing.T) {
	for name, tc := range map[string]struct {
		lifetime time.Duration
		expire   bool
		want     int
	}{
		"unexpired token writes":         {lifetime: time.Hour, want: http.StatusOK},
		"token expiring mid-request 401": {lifetime: 500 * time.Millisecond, expire: true, want: http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			env := testenv.New(t)
			project := seedProject(t, env, "example-project")
			job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			expiresAt := time.Now().UTC().Add(tc.lifetime)
			_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "example-expiring-token", Actor: "worker", AdminActor: db.BootstrapActor, ExpiresAt: &expiresAt})
			require.NoError(t, err)
			var store db.Storage = env.DB
			if tc.expire {
				store = expiringRunObservationStore{Storage: env.DB, expiresAt: expiresAt}
			}
			server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "tok", RequireTokenIdentity: true}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			raw, err := json.Marshal(map[string]any{"actor": "worker", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, httpServer.URL+fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), strings.NewReader(string(raw)))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer example-expiring-token")
			request.Header.Set("Content-Type", "application/json")
			response, err := httpServer.Client().Do(request)
			require.NoError(t, err)
			body := readClose(t, response)
			if tc.want == http.StatusOK {
				require.Truef(t, response.StatusCode >= 200 && response.StatusCode < 300, "status %d: %s", response.StatusCode, body)
			} else {
				require.Equalf(t, tc.want, response.StatusCode, "body: %s", body)
			}
			runs, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
			require.NoError(t, err)
			require.Equal(t, tc.want == http.StatusOK, len(runs) == 1, "only an unexpired credential commits the run")
		})
	}
}
