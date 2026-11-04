package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronAccessCommands(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-test-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	team, _, err := env.DB.CreateTeam(t.Context(), "operators", "owner")
	require.NoError(t, err)
	require.Equal(t, "OK cron-access project=example-project revision=0 define=any observe=any\n", requireCmdOutput(t, env, "--agent", "cron", "access", "show", project.Name))
	out := requireCmdOutput(t, env, "--json", "cron", "access", "set", project.Name, "--define", "teams", "--define-team", "operators")
	var result struct {
		Policy db.ProjectCronAccessPolicy `json:"policy"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, []string{team.UID}, result.Policy.Define.TeamUIDs)
	require.Equal(t, "any", result.Policy.Observe.Mode)
	require.Equal(t, int64(1), result.Policy.Revision)
	require.Equal(t, "OK cron-access project=example-project revision=2 define=teams observe=teams define_teams="+team.UID+"\n", requireCmdOutput(t, env, "--agent", "cron", "access", "set", project.Name, "--observe", "teams"))
	out = requireCmdOutput(t, env, "--json", "cron", "access", "set", project.Name, "--observe-team", team.UID)
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, []string{team.UID}, result.Policy.Observe.TeamUIDs)
	require.Equal(t, []string{team.UID}, result.Policy.Define.TeamUIDs)
	requireCmdOutput(t, env, "cron", "access", "set", project.Name, "--define", "any")
	policy, err := env.DB.CronAccessPolicy(t.Context(), project.UID)
	require.NoError(t, err)
	require.Equal(t, "any", policy.Define.Mode)
	require.Empty(t, policy.Define.TeamUIDs)
	require.Equal(t, []string{team.UID}, policy.Observe.TeamUIDs)
}

// The CLI must preserve a competing edit rather than silently retry stale CAS.
func TestCronAccessCommandConflict(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-test-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	writes := 0
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodGet && response.Request.URL.Path == "/api/v1/projects/"+fmt.Sprint(project.ID)+"/cron/access" {
			policy, err := env.DB.CronAccessPolicy(response.Request.Context(), project.UID)
			if err != nil {
				return err
			}
			policy.Observe = db.CronGrant{Mode: "teams"}
			_, _, err = env.DB.SetCronAccessPolicy(response.Request.Context(), policy, "owner")
			return err
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes++
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	cliEnv := *env
	cliEnv.URL = server.URL
	_, err = runCmdOutput(t, &cliEnv, "--json", "cron", "access", "set", project.Name, "--define", "teams")
	var failure *cliError
	require.True(t, errors.As(err, &failure))
	require.Equal(t, ExitConflict, failure.ExitCode)
	require.Equal(t, "revision_conflict", failure.Code)
	require.Equal(t, 1, writes)
	current, err := env.DB.CronAccessPolicy(t.Context(), project.UID)
	require.NoError(t, err)
	require.Equal(t, "any", current.Define.Mode)
	require.Equal(t, "teams", current.Observe.Mode)
}
