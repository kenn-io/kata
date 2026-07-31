package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationRebindUsesNamedSpokeDaemonAndPostsOnlyCatalogName(t *testing.T) {
	var requestBody map[string]any
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/federation/status":
			writeFederationRebindJSON(t, w, map[string]any{"statuses": []map[string]any{{
				"project_id": 7, "project_name": "spoke-project", "role": "spoke",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/federation/replicas/7/actions/rebind":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
			writeFederationRebindJSON(t, w, federationRebindResponse(7, "spoke-project", "rebound"))
		default:
			http.NotFound(w, r)
		}
	})
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(`
[[daemon]]
name = "spoke-daemon"
url = %q
token = "spoke-daemon-secret"
`, server.URL)), 0o600))

	out, err := runCmdOutput(t, nil,
		"--daemon", "spoke-daemon", "federation", "rebind", "spoke-project",
		"--hub", "primary-hub")

	require.NoError(t, err)
	assert.Equal(t, map[string]any{"hub_catalog": "primary-hub"}, requestBody)
	assert.Contains(t, out, "spoke-project")
	assert.Contains(t, out, "rebound")
	assert.NotContains(t, out, "spoke-daemon-secret")
}

func TestFederationRebindValidatesSelectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "hub required", args: []string{"federation", "rebind", "spoke-project"}, want: `required flag(s) "hub"`},
		{name: "positional and project", args: []string{"--project", "other-project", "federation", "rebind", "spoke-project", "--hub", "primary-hub"}, want: "mutually exclusive"},
		{name: "all and positional", args: []string{"federation", "rebind", "spoke-project", "--all", "--hub", "primary-hub"}, want: "mutually exclusive"},
		{name: "all and project", args: []string{"--project", "spoke-project", "federation", "rebind", "--all", "--hub", "primary-hub"}, want: "mutually exclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCmdOutput(t, nil, tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestFederationRebindAllIsOrderedContinuesAndRendersEveryResult(t *testing.T) {
	var mu sync.Mutex
	var posted []int
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/federation/status":
			writeFederationRebindJSON(t, w, map[string]any{"statuses": []map[string]any{
				{"project_id": 9, "project_name": "spoke-nine", "role": "spoke"},
				{"project_id": 2, "project_name": "hub-project", "role": "hub"},
				{"project_id": 5, "project_name": "spoke-five", "role": "spoke"},
				{"project_id": 3, "project_name": "spoke-three", "role": "spoke"},
			}})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/federation/replicas/"):
			parts := strings.Split(r.URL.Path, "/")
			id, err := strconv.Atoi(parts[5])
			require.NoError(t, err)
			mu.Lock()
			posted = append(posted, id)
			mu.Unlock()
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]any{"hub_catalog": "primary-hub"}, body)
			if id == 5 {
				w.WriteHeader(http.StatusBadGateway)
				writeFederationRebindJSON(t, w, map[string]any{"error": map[string]any{
					"code": "federation_hub_unavailable", "message": "target hub unavailable",
				}})
				return
			}
			state := "unchanged"
			name := "spoke-three"
			if id == 9 {
				state = "resumed"
				name = "spoke-nine"
			}
			writeFederationRebindJSON(t, w, federationRebindResponse(int64(id), name, state))
		default:
			http.NotFound(w, r)
		}
	})

	out, err := runFederationRebindAgainstServer(t, server.URL,
		"--json", "federation", "rebind", "--all", "--hub", "primary-hub")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 of 3 federation rebind operations failed")
	assert.Equal(t, []int{3, 5, 9}, posted)
	var got struct {
		KataAPIVersion int `json:"kata_api_version"`
		Results        []struct {
			Project   string `json:"project"`
			ProjectID int64  `json:"project_id"`
			State     string `json:"state"`
			Error     string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	require.Len(t, got.Results, 3)
	assert.Equal(t, int64(3), got.Results[0].ProjectID)
	assert.Equal(t, "unchanged", got.Results[0].State)
	assert.Equal(t, "spoke-five", got.Results[1].Project)
	assert.Equal(t, "failed", got.Results[1].State)
	assert.Equal(t, "target hub unavailable", got.Results[1].Error)
	assert.Equal(t, int64(9), got.Results[2].ProjectID)
	assert.Equal(t, "resumed", got.Results[2].State)
}

func TestFederationRebindOutputModesDistinguishResumeAndNoop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  string
		state string
		want  string
	}{
		{name: "human resumed", state: "resumed", want: "resumed federation endpoint rebind"},
		{name: "human unchanged", state: "unchanged", want: "federation endpoint already uses"},
		{name: "agent resumed", mode: "--agent", state: "resumed", want: "state=resumed"},
		{name: "agent unchanged", mode: "--agent", state: "unchanged", want: "state=unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/federation/status":
					writeFederationRebindJSON(t, w, map[string]any{"statuses": []map[string]any{{
						"project_id": 7, "project_name": "spoke-project", "role": "spoke",
					}}})
				case r.Method == http.MethodPost:
					writeFederationRebindJSON(t, w, federationRebindResponse(7, "spoke-project", tc.state))
				default:
					http.NotFound(w, r)
				}
			})
			args := []string{"federation", "rebind", "spoke-project", "--hub", "primary-hub"}
			if tc.mode != "" {
				args = append([]string{tc.mode}, args...)
			}

			out, err := runFederationRebindAgainstServer(t, server.URL, args...)

			require.NoError(t, err)
			assert.Contains(t, out, tc.want)
			for _, secret := range []string{"enrollment-secret", "catalog-admin-secret", "spoke-daemon-secret"} {
				assert.NotContains(t, out, secret)
			}
		})
	}
}

func newFederationRebindCLIServer(
	t *testing.T,
	handler func(http.ResponseWriter, *http.Request),
) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/ping" {
			writeFederationRebindJSON(t, w, map[string]any{"ok": true, "service": "kata", "version": "test"})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func runFederationRebindAgainstServer(t *testing.T, serverURL string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("KATA_SERVER", serverURL)
	t.Setenv("KATA_AUTH_TOKEN", "spoke-daemon-secret")
	return runCmdOutput(t, nil, args...)
}

func writeFederationRebindJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

func federationRebindResponse(projectID int64, projectName, state string) map[string]any {
	return map[string]any{
		"project":    map[string]any{"id": projectID, "name": projectName},
		"old_origin": "http://192.0.2.10:7777",
		"new_origin": "https://hub.example",
		"state":      state,
	}
}
