package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"

	"go.kenn.io/kata/internal/version"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubVersionInfo overrides the exported version.* package variables for
// the duration of the test so the version command produces a deterministic
// output regardless of how the test binary was built.
func stubVersionInfo(t *testing.T, ver, commit, built string) {
	t.Helper()
	origVer, origCommit, origBuilt := version.Version, version.Commit, version.BuildDate
	version.Version, version.Commit, version.BuildDate = ver, commit, built
	t.Cleanup(func() {
		version.Version, version.Commit, version.BuildDate = origVer, origCommit, origBuilt
	})
}

func stubDistribution(t *testing.T, distribution string) {
	t.Helper()
	orig := version.Distribution
	version.Distribution = distribution
	t.Cleanup(func() { version.Distribution = orig })
}

func TestVersion_HumanFormatIncludesBuildMetadata(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.0.1-test", "abc1234", "2026-05-12T11:17:12Z")
	stubDistribution(t, "homebrew")

	out := string(executeRoot(t, newVersionCmd()))

	expected := fmt.Sprintf(
		"kata v0.0.1-test\n"+
			"  commit:  abc1234\n"+
			"  built:   2026-05-12T11:17:12Z\n"+
			"  go:      %s\n"+
			"  os/arch: %s/%s\n",
		runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)
	assert.Equal(t, expected, out)
}

func TestVersion_JSONEnvelope(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.0.1-test", "abc1234", "2026-05-12T11:17:12Z")
	stubDistribution(t, "homebrew")
	t.Chdir(t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")

	out := executeRoot(t, newRootCmd(), "version", "--json")

	var got struct {
		APIVersion   int    `json:"kata_api_version"`
		Name         string `json:"name"`
		Version      string `json:"version"`
		Commit       string `json:"commit"`
		Built        string `json:"built"`
		Go           string `json:"go"`
		OS           string `json:"os"`
		Arch         string `json:"arch"`
		Distribution string `json:"distribution"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.APIVersion)
	assert.Equal(t, "kata", got.Name)
	assert.Equal(t, "v0.0.1-test", got.Version)
	assert.Equal(t, "abc1234", got.Commit)
	assert.Equal(t, "2026-05-12T11:17:12Z", got.Built)
	assert.Equal(t, runtime.Version(), got.Go)
	assert.Equal(t, runtime.GOOS, got.OS)
	assert.Equal(t, runtime.GOARCH, got.Arch)
	assert.Equal(t, "homebrew", got.Distribution)
}

func TestVersion_AgentIncludesFormatVersion(t *testing.T) {
	resetFlags(t)
	stubDistribution(t, "homebrew")
	out := string(executeRoot(t, newRootCmd(), "--agent", "version"))
	assert.Equal(t, "OK version version="+agentValue(version.Version)+" agent_format=1\n", out)
}

func TestVersion_JSONIncludesAgentFormat(t *testing.T) {
	resetFlags(t)
	out := executeRoot(t, newRootCmd(), "--json", "version")
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, float64(agentFormatVersion), got["agent_format"])
}

func TestVersion_IsWiredOnRoot(t *testing.T) {
	resetFlags(t)
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Use == "version" {
			return
		}
	}
	t.Fatal("version subcommand not registered on root")
}

// The conventional `--version` entry point must produce exactly what the
// `version` subcommand produces, so tooling that probes either spelling gets
// the same answer.
func TestVersion_RootFlagMatchesSubcommand(t *testing.T) {
	stubVersionInfo(t, "v0.0.1-test", "abc1234", "2026-05-12T11:17:12Z")

	want := string(executeRoot(t, newRootCmd(), "version"))
	require.Contains(t, want, "kata v0.0.1-test")
	assert.Equal(t, want, string(executeRoot(t, newRootCmd(), "--version")))
}

// --version deliberately has no shorthand: -v conventionally means verbose,
// and reclaiming it after release would be a breaking change.
func TestVersion_RootFlagHasNoShorthand(t *testing.T) {
	assert.Empty(t, newRootCmd().Flags().Lookup("version").Shorthand)

	_, _, err := executeRootCapture(t, context.Background(), "-v")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown shorthand flag: 'v'")
}

func TestVersion_RootFlagHonorsOutputMode(t *testing.T) {
	stubVersionInfo(t, "v0.0.1-test", "abc1234", "2026-05-12T11:17:12Z")

	out := executeRoot(t, newRootCmd(), "--version", "--json")
	var got struct {
		APIVersion int    `json:"kata_api_version"`
		Name       string `json:"name"`
		Version    string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.APIVersion)
	assert.Equal(t, "kata", got.Name)
	assert.Equal(t, "v0.0.1-test", got.Version)

	agentOut := string(executeRoot(t, newRootCmd(), "--version", "--agent"))
	assert.Contains(t, agentOut, "OK version version=v0.0.1-test")
	assert.Contains(t, agentOut, "agent_format=1")
}

// Wiring RunE onto the root command to serve --version must not swallow the
// default no-args behavior: bare `kata` still prints help and exits zero.
func TestRoot_NoArgsPrintsHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--json"}} {
		out := string(executeRoot(t, newRootCmd(), args...))
		assert.Contains(t, out, "lightweight issue tracker")
		assert.Contains(t, out, "Available Commands:")
	}
}

// A runnable root runs PersistentPreRunE for a bare `kata`, which cobra
// previously skipped by short-circuiting to help. Global output-mode
// validation therefore now applies to the root itself; pin that contract so
// it stays deliberate rather than incidental.
func TestRoot_NoArgsValidatesGlobalOutputFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "conflicting modes", args: []string{"--json", "--agent"}, want: "conflicting output modes"},
		{name: "unsupported format", args: []string{"--format", "bogus"}, want: `unsupported output format "bogus"`},
		// With --version present, PersistentPreRunE fails before flags.Mode
		// is set, so emitRootError re-scans raw argv. That scan must treat
		// --version as a valueless bool; if it ever consumed a value it
		// would swallow the following --format value here.
		{name: "conflicting modes with flag", args: []string{"--version", "--json", "--agent"}, want: "conflicting output modes"},
		{name: "unsupported format with flag", args: []string{"--version", "--format", "bogus"}, want: `unsupported output format "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := executeRootCapture(t, context.Background(), tc.args...)
			require.Error(t, err)
			var cli *cliError
			require.ErrorAs(t, err, &cli)
			assert.Equal(t, ExitUsage, cli.ExitCode)
			assert.Contains(t, stderr, tc.want)
		})
	}
}

// The root --version flag must stay root-local. `kata openapi --version 3.0`
// is a documented release step whose own --version takes a value, so a
// persistent root flag would silently break it.
func TestVersion_RootFlagDoesNotShadowSubcommandFlag(t *testing.T) {
	out := string(executeRoot(t, newRootCmd(), "openapi", "--version", "3.0", "--format", "yaml"))
	assert.Contains(t, out, "openapi: 3.0.3")

	_, _, err := executeRootCapture(t, context.Background(), "list", "--version")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag: --version")
}

// An unknown command must stay a usage error rather than falling through to
// the root's --version handler.
func TestRoot_UnknownCommandStillFails(t *testing.T) {
	_, _, err := executeRootCapture(t, context.Background(), "definitely-not-a-command")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}
