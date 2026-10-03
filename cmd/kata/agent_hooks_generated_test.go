package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// These fixed artifacts use a different runtime and package version from the
// current generator. Their digests cover the exact metadata and artifact bytes.
func TestNativeGeneratedArtifactsSurviveGeneratorChanges(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	for _, agent := range []string{"pi", "amp", "opencode-v1", "opencode-v2", "openclaw"} {
		for _, operation := range []string{"install", "uninstall", "status", "edited-code", "edited-metadata", "edited-package"} {
			t.Run(agent+"/"+operation, func(t *testing.T) {
				root := t.TempDir()
				opts := nativeAgentHookOptions{Home: root, Dir: root, Scope: "user", Executable: "kata", Contract: true, Attention: true}
				planner := planPiAgentHooks
				switch agent {
				case "amp":
					planner = planAmpAgentHooks
				case "opencode-v1", "opencode-v2":
					planner = planOpenCodeAgentHooks
					opts.API = agent[len("opencode-"):]
				case "openclaw":
					planner = planOpenClawAgentHooks
				}
				initial, err := planner(opts, false)
				require.NoError(t, err)
				path := initial.Path
				if agent == "openclaw" {
					path = initial.Changes[0].Path
				}
				artifact, err := os.ReadFile(filepath.Join("testdata", "agent-hooks", "ownership", agent+".js")) //nolint:gosec // G304: agent is selected from the fixed fixture table above.
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
				switch operation {
				case "edited-code":
					artifact = bytes.Replace(artifact, []byte("export default"), []byte("// authored\nexport default"), 1)
				case "edited-metadata":
					artifact = bytes.Replace(artifact, []byte(`"contract":true`), []byte(`"contract":false`), 1)
				}
				require.NoError(t, os.WriteFile(path, artifact, 0600)) //nolint:gosec // G703: planner uses only the temporary test home.
				companions := map[string]string{}
				if agent == "opencode-v2" || agent == "openclaw" {
					companions["package.json"] = agent + ".package.json"
				}
				if agent == "openclaw" {
					companions["openclaw.plugin.json"] = "openclaw.plugin.json"
				}
				if operation == "edited-package" && len(companions) == 0 {
					t.Skip("single-file extension")
				}
				for name, fixture := range companions {
					data, err := os.ReadFile(filepath.Join("testdata", "agent-hooks", "ownership", fixture)) //nolint:gosec // G304: fixture is selected from the fixed companion table above.
					require.NoError(t, err)
					if operation == "edited-package" {
						data = append(data, '\n')
					}
					require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), name), data, 0600)) //nolint:gosec // G703: companions are adjacent to the temporary planned extension.
				}
				if operation == "status" {
					opts.Contract, opts.Attention = false, false
				}
				plan, err := planner(opts, operation != "install")
				if operation == "edited-code" || operation == "edited-metadata" || operation == "edited-package" {
					require.Error(t, err)
					retained, err := os.ReadFile(path) //nolint:gosec // G304: extension is in the temporary test home.
					require.NoError(t, err)
					require.Equal(t, artifact, retained)
					return
				}
				require.NoError(t, err)
				changed, err := publishNativeAgentHookPlan(plan)
				require.NoError(t, err)
				if operation == "status" {
					require.False(t, changed, "status rewrote an older generated artifact")
					return
				}
				require.True(t, changed)
				if operation == "uninstall" {
					_, err = os.Stat(path)
					require.True(t, os.IsNotExist(err), "uninstall retained an owned artifact")
				} else {
					reinstalled, err := os.ReadFile(path) //nolint:gosec // G304: extension is in the temporary test home.
					require.NoError(t, err)
					require.NotEqual(t, artifact, reinstalled, "install did not upgrade the runtime")
					_, err = planner(opts, true)
					require.NoError(t, err, "upgraded artifact lost ownership")
				}
			})
		}
	}
}
