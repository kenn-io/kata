package main

import (
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

func TestVersion_HumanFormatMatchesMsgvault(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.0.1-test", "abc1234", "2026-05-12T11:17:12Z")

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
	flags.JSON = true
	stubVersionInfo(t, "v0.0.1-test", "abc1234", "2026-05-12T11:17:12Z")

	out := executeRoot(t, newVersionCmd())

	var got struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Built   string `json:"built"`
		Go      string `json:"go"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "v0.0.1-test", got.Version)
	assert.Equal(t, "abc1234", got.Commit)
	assert.Equal(t, "2026-05-12T11:17:12Z", got.Built)
	assert.Equal(t, runtime.Version(), got.Go)
	assert.Equal(t, runtime.GOOS, got.OS)
	assert.Equal(t, runtime.GOARCH, got.Arch)
}

func TestVersion_AgentIncludesFormatVersion(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "--agent", "version"))
	assert.Contains(t, out, "OK version ")
	assert.Contains(t, out, "agent_format=1")
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
