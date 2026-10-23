package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
)

func TestFederationEnrollBootstrapScoped(t *testing.T) {
	for _, selector := range []string{"positional", "flag", "workspace", "renamed workspace with reused name"} {
		t.Run(selector, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("owner-fixture"), testenv.WithRequireTokenIdentity())
			t.Setenv("TEST_ENROLL_OWNER", "owner-fixture")
			project, err := env.DB.CreateProject(t.Context(), "hub-project")
			require.NoError(t, err)
			_, err = env.DB.EnableProjectFederation(t.Context(), project.ID, "member")
			require.NoError(t, err)
			_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "account-fixture", Actor: "member", AdminActor: "bootstrap"})
			require.NoError(t, err)
			dir := t.TempDir()
			if selector == "workspace" {
				require.NoError(t, config.WriteProjectConfig(dir, project.Name))
				testfix.MkDotGit(t, dir)
			}
			if selector == "renamed workspace with reused name" {
				require.NoError(t, config.WriteProjectConfig(dir, project.Name))
				_, err = env.DB.AttachAlias(t.Context(), project.ID, "local://"+dir, "local")
				require.NoError(t, err)
				_, err = env.DB.RenameProject(t.Context(), project.ID, "renamed-project")
				require.NoError(t, err)
				reused, err := env.DB.CreateProject(t.Context(), project.Name)
				require.NoError(t, err)
				require.NotEqual(t, project.ID, reused.ID)
			}
			args := []string{"federation", "enroll", "--spoke-instance", env.DB.InstanceUID(), "--hub-url", env.URL, "--hub-token-env", "TEST_ENROLL_OWNER", "--actor", "member", "--capabilities", "pull", "--json"}
			if selector == "positional" {
				args = append(args, project.Name)
			}
			if selector == "flag" {
				args = append(args, "--project", project.Name)
			}
			out := runCLI(t, env, dir, args...)
			assert.Contains(t, out, project.UID)
			assert.Contains(t, out, "federation join")
			var result struct {
				Enrollment struct {
					ID        int64  `json:"id"`
					Actor     string `json:"actor"`
					ProjectID *int64 `json:"project_id"`
					Token     string `json:"token"`
				} `json:"enrollment"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			assert.Positive(t, result.Enrollment.ID)
			assert.Equal(t, "member", result.Enrollment.Actor)
			require.NotNil(t, result.Enrollment.ProjectID)
			assert.Equal(t, project.ID, *result.Enrollment.ProjectID)
			assert.NotEmpty(t, result.Enrollment.Token)
			if selector == "renamed workspace with reused name" {
				assert.Contains(t, out, "renamed-project")
				enrollments, err := env.DB.ListFederationEnrollments(t.Context())
				require.NoError(t, err)
				require.Len(t, enrollments, 1)
				assert.Equal(t, &project.ID, enrollments[0].ProjectID)
			}
		})
	}
}

func TestFederationEnrollBootstrapRequiresEnabledHub(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-fixture"), testenv.WithRequireTokenIdentity())
	t.Setenv("TEST_ENROLL_OWNER", "owner-fixture")
	project, err := env.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = runCLICapture(t, env, t.TempDir(), "federation", "enroll", project.Name, "--spoke-instance", env.DB.InstanceUID(), "--hub-url", env.URL, "--hub-token-env", "TEST_ENROLL_OWNER", "--actor", "member", "--capabilities", "pull")
	require.Error(t, err)
	var failure *cliError
	require.ErrorAs(t, err, &failure)
	assert.Contains(t, failure.Hint, "kata federation enable")
	assert.Contains(t, failure.Hint, "account token")
}

func TestFederationEnrollProjectFallbackRequiresNotFoundStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"forbidden", http.StatusForbidden, "project_access_denied"},
		{"other not found", http.StatusNotFound, "project_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			flags.Project = "example-project"
			initialized := false
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/projects/resolve" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"status":` + strconv.Itoa(tc.status) + `,"error":{"code":"` + tc.code + `","message":"request failed"}}`))
					return
				}
				initialized = true
				_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
			}))
			defer hub.Close()
			_, err := resolveFederationProject(hubAPI(t.Context(), hub.URL, hub.Client()), nil, true, "member")
			assert.False(t, initialized, "a non-missing resolve failure must not fall back to project init")
			require.Error(t, err)
		})
	}
}
