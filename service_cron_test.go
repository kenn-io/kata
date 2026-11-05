package kata

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestServiceCronExecutionConfig(t *testing.T) {
	for _, mode := range []string{"", "owned", "invalid", "project"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			cfg := Config{DSN: filepath.Join(t.TempDir(), "db"), Auth: AuthConfig{TrustCallerAuthentication: true}, CronExecution: CronExecutionConfig{ReplicaExecute: mode}}
			if mode == "project" {
				cfg.CronExecution = CronExecutionConfig{ReplicaExecute: "none", Projects: []CronExecutionProjectConfig{{Name: "company-project", Execute: "all"}}}
			}
			service, err := New(t.Context(), cfg)
			if mode == "invalid" {
				require.ErrorContains(t, err, "cron.replica_execute")
				require.True(t, strings.HasPrefix(err.Error(), "kata:"), "%s", err)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			project, err := service.store.CreateProject(t.Context(), "company-project")
			require.NoError(t, err)
			_, err = service.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			service.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil))
			require.Equalf(t, 200, recorder.Code, "%s", recorder.Body.String())
			var result struct {
				Execution struct {
					Mode string `json:"mode"`
				} `json:"execution"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
			want := mode
			if mode == "" {
				want = "none"
			}
			if mode == "project" {
				want = "all"
			}
			require.Equal(t, want, result.Execution.Mode)
		})
	}
}
