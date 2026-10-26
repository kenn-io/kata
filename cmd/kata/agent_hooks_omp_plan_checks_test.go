package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestOmpPlanRefusesEnabledTodo(t *testing.T) {
	home := isolateAgentHookHomes(t)
	fakeOmpPlan(t, home, `{"value":true}`, `{"marketplace":[]}`)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "omp", "--plan", "--executable", os.Args[0])
	require.EqualError(t, err, "omp's built-in todo is enabled; run omp config set todo.enabled false before rerunning this command (Kata does not change omp's settings)")
	_, err = os.Stat(filepath.Join(home, "extensions"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestOmpPlanRefusesInstalledPlugin(t *testing.T) {
	for _, id := range []string{"kata@kata-plugin"} {
		t.Run(id, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			fakeOmpPlan(t, home, `{"value":false}`, `{"marketplace":[{"id":"`+id+`","enabled":true}]}`)
			_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "omp", "--plan", "--executable", os.Args[0])
			require.EqualError(t, err, "the kata-plugin omp plugin also provides a todo tool; remove it with omp plugin uninstall kata@kata-plugin")
			_, err = os.Stat(filepath.Join(home, "extensions"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestOmpPlanAllowsOtherMarketplace(t *testing.T) {
	home := isolateAgentHookHomes(t)
	fakeOmpPlan(t, home, `{"value":false}`, `{"marketplace":[{"id":"kata@another-marketplace"}]}`)
	ompPlanCLI(t, "install", "--plan")
	_, err := os.Stat(filepath.Join(home, "extensions", "kata.js"))
	require.NoError(t, err)
}

func TestOmpPlanRefusesUnparseableConfigOutput(t *testing.T) {
	for _, config := range []string{"broken", "null", `{}`, `{"value":null}`, `{"value":"false"}`, `{"value":0}`} {
		t.Run(config, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			fakeOmpPlan(t, home, config, `{"marketplace":[]}`)
			_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "omp", "--plan", "--executable", os.Args[0])
			require.Error(t, err)
			_, err = os.Stat(filepath.Join(home, "extensions"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestOmpPlanUninstallNote(t *testing.T) {
	for _, extra := range [][]string{nil, {"--plan"}} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			fakeOmpPlan(t, home, `{"value":false}`, `{"marketplace":[{"id":"kata@kata-plugin","enabled":false}]}`)
			ompPlanCLI(t, "install", "--plan")
			require.Contains(t, ompPlanCLI(t, "uninstall", extra...), "omp's built-in todo stays disabled; re-enable it with omp config reset todo.enabled.")
		})
	}
}

func TestOmpNeverWritesConfigProperty(t *testing.T) {
	home := isolateAgentHookHomes(t)
	hegel.Test(t, func(ht *hegel.T) {
		// Limit filesystem work at materialization; the generator retains its
		// full sequence domain and shrinking can still reach the empty case.
		ops := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 31)))
		if len(ops) > 16 {
			ops = ops[:16]
		}
		path := filepath.Join(home, "extensions", "kata.js")
		require.NoError(ompPropertyT{ht}, os.RemoveAll(filepath.Dir(path)))
		for _, op := range ops {
			config := `{"value":false}`
			if op&8 != 0 {
				config = `{"value":true}`
			}
			plugins := `{"marketplace":[]}`
			if op&16 != 0 {
				plugins = `{"marketplace":[{"id":"kata@kata-plugin"}]}`
			}
			calls := fakeOmpPlan(t, home, config, plugins)
			before, beforeErr := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
			resetFlags(t)
			verb := "install"
			if op&1 != 0 {
				verb = "uninstall"
			}
			args := []string{"agent-hook", verb, "omp"}
			if op&2 != 0 {
				args = append(args, "--plan")
			}
			if op&4 != 0 {
				args = append(args, "--contract-only")
			}
			if verb == "install" {
				args = append(args, "--executable", os.Args[0])
			}
			_, _, err := executeAgentHook(t, strings.NewReader(""), args...)
			if verb == "install" && op&2 != 0 && op&24 != 0 {
				require.Error(ompPropertyT{ht}, err)
				after, afterErr := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
				require.Equal(ompPropertyT{ht}, os.IsNotExist(beforeErr), os.IsNotExist(afterErr))
				require.Equal(ompPropertyT{ht}, before, after)
			} else {
				require.NoError(ompPropertyT{ht}, err)
			}
			for _, call := range *calls {
				require.False(ompPropertyT{ht}, len(call) > 1 && call[0] == "config" && (call[1] == "set" || call[1] == "reset"), "config mutation: %v", call)
			}
		}
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}
