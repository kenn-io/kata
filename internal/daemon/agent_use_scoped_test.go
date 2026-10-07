package daemon_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type scopedAgentReporter struct{ events []string }

func (*scopedAgentReporter) Enabled() bool            { return true }
func (*scopedAgentReporter) EventAllowed(string) bool { return true }
func (*scopedAgentReporter) SanitizeProperties(string, map[string]any) (map[string]any, error) {
	return nil, nil
}
func (r *scopedAgentReporter) Capture(event string, _ map[string]any) error {
	r.events = append(r.events, event)
	return nil
}

func TestScopedAgentTelemetryAdmission(t *testing.T) {
	r := &scopedAgentReporter{}
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(), func(cfg *daemon.ServerConfig) { cfg.Telemetry = r })
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID},
		ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
	})
	require.NoError(t, err)
	for _, test := range []struct {
		event  string
		status int
	}{
		{"agent_active", http.StatusAccepted},
		{"app_opened", http.StatusForbidden}, {"agent_call_count", http.StatusForbidden},
	} {
		resp, body := envDoRaw(t, env, http.MethodPost, "/api/v1/ui/telemetry", map[string]any{"event": test.event, "properties": map[string]any{"call_count_bucket": "over-100"}}, map[string]string{"Authorization": "Bearer worker-token"})
		require.Equal(t, test.status, resp.StatusCode, string(body))
	}
	require.Equal(t, []string{"agent_active"}, r.events)
}
