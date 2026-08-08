package scripts_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type releaseWorkflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestReleaseWorkflowInstallsWebDependenciesWithoutStagingAssets(t *testing.T) {
	contents, err := os.ReadFile("../.github/workflows/release.yml")
	require.NoError(t, err)

	var workflow releaseWorkflow
	require.NoError(t, yaml.Unmarshal(contents, &workflow))
	steps := workflow.Jobs["publish"].Steps
	require.NotEmpty(t, steps)

	dependencyStep := -1
	goreleaserStep := -1
	for index, step := range steps {
		switch step.Name {
		case "Install release web dependencies":
			dependencyStep = index
			require.Equal(t, "make web-install", strings.TrimSpace(step.Run))
		case "Run GoReleaser":
			goreleaserStep = index
		}
	}

	require.NotEqual(t, -1, dependencyStep, "release workflow must install frozen web dependencies")
	require.NotEqual(t, -1, goreleaserStep, "release workflow must run GoReleaser")
	require.Less(t, dependencyStep, goreleaserStep)
	for _, step := range steps[:goreleaserStep] {
		require.NotContains(t, step.Run, "web-release-check",
			"pre-GoReleaser steps must leave embedded web assets untouched for the clean-tree gate")
	}
}
