package client_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/katatest"
)

func TestMain(m *testing.M) {
	if argsFile := os.Getenv("FAKE_KATA_ARGS_FILE"); argsFile != "" {
		os.Exit(fakeKata(argsFile))
	}
	os.Exit(m.Run())
}

// fakeKata stands in for the kata executable StartWith names. It records its
// arguments, then either fails with FAKE_KATA_OUTPUT on stderr, or "starts" a
// daemon by publishing the runtime record of a server the test already runs.
func fakeKata(argsFile string) int {
	if err := os.WriteFile(argsFile, []byte(strings.Join(os.Args[1:], "\n")), 0o600); err != nil { //nolint:gosec // G703: the parent test chose this temporary path.
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if output := os.Getenv("FAKE_KATA_OUTPUT"); output != "" {
		fmt.Fprintln(os.Stderr, output)
		return 7
	}
	_, err := (kitdaemon.RuntimeStore{Dir: os.Getenv("FAKE_KATA_RUNTIME_DIR")}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getppid(),
		Address:   os.Getenv("FAKE_KATA_ADDRESS"),
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}

// fakeKataStarts makes the test binary a kata executable whose start command
// publishes address in namespace. Like kata, it creates the runtime directory
// itself: a directory made beforehand lacks the private permissions Windows
// discovery requires. It returns the executable and a function reporting the
// arguments it was run with, or nil when it never ran.
func fakeKataStarts(t *testing.T, namespace *daemonruntime.Namespace, address string) (string, func() []string) {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_KATA_ARGS_FILE", argsFile)
	t.Setenv("FAKE_KATA_RUNTIME_DIR", namespace.DataDir)
	t.Setenv("FAKE_KATA_ADDRESS", address)
	executable, err := os.Executable()
	require.NoError(t, err)
	return executable, func() []string {
		data, err := os.ReadFile(argsFile) //nolint:gosec // G304: the test's own temporary file.
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(string(data), "\n")
	}
}

func TestDiscoverStartsStoppedNamedLocalDaemon(t *testing.T) {
	home := isolateKataHome(t)
	local := katatest.New(t, katatest.WithUnixSocket())
	initProject(t, local.Client(t), "spoke-project")
	writeDaemonCatalog(t, home, "[[daemon]]\nname = \"personal\"\nlocal = true\n")
	namespace, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	kata, ranWith := fakeKataStarts(t, namespace, local.Endpoint)

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "personal", StartWith: kata})
	require.NoError(t, err)

	assert.Equal(t, []string{"daemon", "start"}, ranWith())
	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "spoke-project", projects[0].Name)
}

func TestDiscoverRecoversStoppedNamedProfile(t *testing.T) {
	home := isolateKataHome(t)
	work := katatest.New(t, katatest.WithUnixSocket())
	initProject(t, work.Client(t), "work-project")
	kata, ranWith := fakeKataStarts(t, pinnedProfile(t, home, work), work.Endpoint)

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "work", StartWith: kata})
	require.NoError(t, err)

	assert.Equal(t, []string{"--daemon", "work", "daemon", "recover"}, ranWith(),
		"recover verifies the profile's storage and instance before starting it")
	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "work-project", projects[0].Name)
}

func TestDiscoverRecoversStoppedWorkspaceProfile(t *testing.T) {
	home := isolateKataHome(t)
	work := katatest.New(t, katatest.WithUnixSocket())
	initProject(t, work.Client(t), "work-project")
	kata, ranWith := fakeKataStarts(t, pinnedProfile(t, home, work), work.Endpoint)
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte(
		"version = 1\n\n[project]\nidentity = \"example.test/work-project\"\nname = \"work-project\"\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
		[]byte("version = 1\n\n[server]\ndaemon = \"work\"\n"), 0o600))

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Workspace: workspace, StartWith: kata})
	require.NoError(t, err)

	assert.Equal(t, []string{"--workspace", workspace, "daemon", "recover"}, ranWith(),
		"kata must select the profile from the same workspace")
	projects := listProjects(t, api)
	require.Len(t, projects, 1)
	assert.Equal(t, "work-project", projects[0].Name)
}

func TestDiscoverReportsKataStartFailure(t *testing.T) {
	home := isolateKataHome(t)
	work := katatest.New(t, katatest.WithUnixSocket())
	kata, ranWith := fakeKataStarts(t, pinnedProfile(t, home, work), work.Endpoint)
	t.Setenv("FAKE_KATA_OUTPUT", "kata: missing_profile_storage: profile storage is missing")

	_, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "work", StartWith: kata})

	require.ErrorIs(t, err, client.ErrDaemonUnavailable)
	assert.Contains(t, err.Error(), "missing_profile_storage: profile storage is missing")
	assert.Equal(t, []string{"--daemon", "work", "daemon", "recover"}, ranWith())
}

func TestDiscoverNeverStartsRemote(t *testing.T) {
	home := isolateKataHome(t)
	writeDaemonCatalog(t, home, "[[daemon]]\nname = \"hub\"\nurl = \"http://127.0.0.1:1\"\n")
	namespace, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	kata, ranWith := fakeKataStarts(t, namespace, "unix://"+filepath.Join(home, "unused.sock"))

	api, err := client.Discover(t.Context(), client.DiscoverOptions{Daemon: "hub", StartWith: kata})
	require.NoError(t, err)

	assert.Nil(t, ranWith())
	discovered, _ := api.Discovered()
	assert.True(t, discovered.Remote)
}
