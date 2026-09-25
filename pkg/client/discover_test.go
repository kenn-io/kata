package client_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

// isolateKataHome points discovery at an empty Kata home and a workspace with
// no .kata.local.toml, so the host's daemons and remote config are invisible.
func isolateKataHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Chdir(t.TempDir())
	return home
}

func publishLocalRuntime(t *testing.T, home, address string) {
	t.Helper()
	namespace, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, namespace.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getpid(),
		Address:   address,
		Metadata:  map[string]string{"db_path": filepath.Join(home, "kata.db")},
		StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
}

func listProjects(t *testing.T, api *client.Client) []generated.ProjectOut {
	t.Helper()
	resp, err := api.ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp.JSON200.Projects
}

func initProject(t *testing.T, api *client.Client, name string) {
	t.Helper()
	actor := "tester"
	resp, err := api.InitProjectWithResponse(t.Context(), &generated.InitProjectRequestOptions{
		Body: &generated.InitProjectBody{Name: &name, Actor: &actor},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDiscoverUsesRunningLocalDaemonWithGlobalToken(t *testing.T) {
	home := isolateKataHome(t)
	server := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("local-token"))
	initProject(t, server.Client(t), "spoke-project")
	publishLocalRuntime(t, home, server.Endpoint)
	t.Setenv("KATA_AUTH_TOKEN", "local-token")

	api, err := client.Discover(t.Context(), client.DiscoverOptions{})
	require.NoError(t, err)

	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "spoke-project", projects[0].Name)
}

func TestDiscoverReportsNoDaemonWithoutStartingOne(t *testing.T) {
	home := isolateKataHome(t)

	_, err := client.Discover(t.Context(), client.DiscoverOptions{})
	require.ErrorIs(t, err, client.ErrDaemonUnavailable)

	namespace, err := daemon.NewNamespace()
	require.NoError(t, err)
	records, err := (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).List()
	require.NoError(t, err)
	assert.Empty(t, records, "discovery must not start a daemon in %s", home)
}

func TestDiscoverReportsUnreachableLocalDaemonAsUnavailable(t *testing.T) {
	home := isolateKataHome(t)
	publishLocalRuntime(t, home, "unix://"+filepath.Join(home, "missing.sock"))

	_, err := client.Discover(t.Context(), client.DiscoverOptions{})
	require.ErrorIs(t, err, client.ErrDaemonUnavailable)
	assert.Contains(t, err.Error(), "missing.sock")
}

func TestDiscoverPrefersKataServerOverLocalDaemon(t *testing.T) {
	home := isolateKataHome(t)
	local := katatest.New(t, katatest.WithUnixSocket())
	initProject(t, local.Client(t), "local-project")
	publishLocalRuntime(t, home, local.Endpoint)
	remote := katatest.New(t, katatest.WithToken("remote-token"))
	initProject(t, remote.Client(t), "hub-project")
	t.Setenv("KATA_SERVER", remote.Endpoint)
	t.Setenv("KATA_AUTH_TOKEN", "remote-token")

	api, err := client.Discover(t.Context(), client.DiscoverOptions{})
	require.NoError(t, err)

	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "hub-project", projects[0].Name)
}

func TestDiscoverReadsLocalConfigFromWorkspace(t *testing.T) {
	isolateKataHome(t)
	remote := katatest.New(t)
	initProject(t, remote.Client(t), "hub-project")
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte(
		"version = 1\n\n[project]\nidentity = \"example.test/spoke-project\"\nname = \"spoke-project\"\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
		[]byte("version = 1\n\n[server]\nurl = \""+remote.Endpoint+"\"\n"), 0o600))

	_, err := client.Discover(t.Context(), client.DiscoverOptions{})
	require.ErrorIs(t, err, client.ErrDaemonUnavailable, "the process working directory has no local config")

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Workspace: workspace})
	require.NoError(t, err)
	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "hub-project", projects[0].Name)
}
