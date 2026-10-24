package client_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
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
	namespace, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	publishRuntime(t, namespace, address, map[string]string{"db_path": filepath.Join(home, "kata.db")})
}

func publishRuntime(t *testing.T, namespace *daemonruntime.Namespace, address string, metadata map[string]string) {
	t.Helper()
	require.NoError(t, namespace.EnsureDirs())
	_, err := (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getpid(),
		Address:   address,
		Metadata:  metadata,
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
	discovered, ok := api.Discovered()
	require.True(t, ok)
	assert.Equal(t, client.DiscoveredDaemon{Remote: false}, discovered)
}

func TestDiscoverReportsNoDaemonWithoutStartingOne(t *testing.T) {
	home := isolateKataHome(t)

	_, err := client.Discover(t.Context(), client.DiscoverOptions{})
	require.ErrorIs(t, err, client.ErrDaemonUnavailable)

	namespace, err := daemonruntime.NewNamespace()
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
	discovered, ok := api.Discovered()
	require.True(t, ok)
	assert.Equal(t, client.DiscoveredDaemon{Remote: true}, discovered)
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

func TestDiscoveredIsAbsentForExplicitEndpointClient(t *testing.T) {
	server := katatest.New(t)

	_, ok := server.Client(t).Discovered()
	assert.False(t, ok)
}

func TestDiscoverNamedRemoteIgnoresOtherSourcesAndUsesEntryToken(t *testing.T) {
	home := isolateKataHome(t)
	other := katatest.New(t)
	initProject(t, other.Client(t), "other-project")
	t.Setenv("KATA_SERVER", other.Endpoint)
	hub := katatest.New(t, katatest.WithToken("hub-token"))
	initProject(t, hub.Client(t), "hub-project")
	t.Setenv("EXAMPLE_HUB_TOKEN", "hub-token")
	writeDaemonCatalog(t, home, `[[daemon]]
name = "hub"
url = "`+hub.Endpoint+`"
token_env = "EXAMPLE_HUB_TOKEN"
`)

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "hub"})
	require.NoError(t, err)

	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "hub-project", projects[0].Name)
	discovered, ok := api.Discovered()
	require.True(t, ok)
	assert.Equal(t, client.DiscoveredDaemon{Remote: true}, discovered)
}

func TestDiscoverNamedLocalOverridesActiveDaemon(t *testing.T) {
	home := isolateKataHome(t)
	local := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("local-token"))
	initProject(t, local.Client(t), "spoke-project")
	publishLocalRuntime(t, home, local.Endpoint)
	writeDaemonCatalog(t, home, `active_daemon = "hub"

[[daemon]]
name = "hub"
url = "http://127.0.0.1:1"

[[daemon]]
name = "personal"
local = true
token = "local-token"
`)

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "personal"})
	require.NoError(t, err)

	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "spoke-project", projects[0].Name)
	discovered, ok := api.Discovered()
	require.True(t, ok)
	assert.Equal(t, client.DiscoveredDaemon{Remote: false}, discovered)
}

// pinnedProfile registers server as the catalog entry "work", a local profile
// pinned to a temporary home and the server's instance UID, and returns the
// namespace where that profile's runtime records live.
func pinnedProfile(t *testing.T, home string, server *katatest.Server) *daemonruntime.Namespace {
	t.Helper()
	instance, err := server.Client(t).InstanceWithResponse(t.Context())
	require.NoError(t, err)
	require.NotNil(t, instance.JSON200)
	entry := config.CatalogDaemonConfig{
		Name: "work", Local: true, Home: t.TempDir(), InstanceUID: instance.JSON200.InstanceUID,
	}
	writeDaemonCatalog(t, home, fmt.Sprintf("[[daemon]]\nname = %q\nlocal = true\nhome = %q\ninstance_uid = %q\n",
		entry.Name, entry.Home, entry.InstanceUID))
	profile, err := config.ResolveLocalProfile(entry)
	require.NoError(t, err)
	namespace, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	return namespace
}

func TestDiscoverNamedPinnedLocalProfile(t *testing.T) {
	home := isolateKataHome(t)
	work := katatest.New(t, katatest.WithUnixSocket())
	initProject(t, work.Client(t), "work-project")
	publishRuntime(t, pinnedProfile(t, home, work), work.Endpoint, nil)

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "work"})
	require.NoError(t, err)

	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "work-project", projects[0].Name)
	discovered, ok := api.Discovered()
	require.True(t, ok)
	assert.Equal(t, client.DiscoveredDaemon{Remote: false}, discovered)
}

func TestDiscoverNamedStoppedLocalIsUnavailableWithoutStartingOne(t *testing.T) {
	home := isolateKataHome(t)
	writeDaemonCatalog(t, home, "[[daemon]]\nname = \"personal\"\nlocal = true\n")

	_, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "personal"})
	require.ErrorIs(t, err, client.ErrDaemonUnavailable)

	namespace, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	records, err := (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).List()
	require.NoError(t, err)
	assert.Empty(t, records, "discovery must not start a daemon in %s", home)
}

func TestDiscoverNamedRemoteIsNotProbed(t *testing.T) {
	home := isolateKataHome(t)
	writeDaemonCatalog(t, home, "[[daemon]]\nname = \"hub\"\nurl = \"http://127.0.0.1:1\"\n")

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "hub"})
	require.NoError(t, err, "like workspace discovery, a named remote is not probed")
	discovered, ok := api.Discovered()
	require.True(t, ok)
	assert.Equal(t, client.DiscoveredDaemon{Remote: true}, discovered)

	_, err = api.PingWithResponse(t.Context())
	require.Error(t, err, "the first request reports the unreachable remote")
}

func TestDiscoverNamedRejectsUnknownName(t *testing.T) {
	home := isolateKataHome(t)
	writeDaemonCatalog(t, home, "[[daemon]]\nname = \"hub\"\nurl = \"https://daemon.example\"\n")

	_, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "example-workspace"})
	require.ErrorIs(t, err, client.ErrDaemonNotInCatalog)
	assert.NotErrorIs(t, err, client.ErrDaemonUnavailable)
}

func TestDiscoverNamedRemoteRequiresTokenEnvValue(t *testing.T) {
	home := isolateKataHome(t)
	hub := katatest.New(t, katatest.WithToken("hub-token"))
	t.Setenv("EXAMPLE_HUB_TOKEN", "")
	writeDaemonCatalog(t, home, `[[daemon]]
name = "hub"
url = "`+hub.Endpoint+`"
token_env = "EXAMPLE_HUB_TOKEN"
`)

	_, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "hub"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EXAMPLE_HUB_TOKEN", "the error names the variable to set")
	assert.NotErrorIs(t, err, client.ErrDaemonUnavailable)
}
