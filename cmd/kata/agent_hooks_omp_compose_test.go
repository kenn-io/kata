package main

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/cmd/kata/agenthookjs"
	"hegel.dev/go/hegel"
)

// A real executable recorder keeps execFile/env tests portable: the test binary
// handles only this explicitly selected fixture mode before running any tests.
func runPlanSubprocessFixture() {
	args := os.Args[1:]
	statePath := os.Getenv("KATA_NATIVE_PLAN_STATE")
	raw, err := os.ReadFile(statePath) //nolint:gosec // G304: subprocess state path is set by the fixture under t.TempDir.
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var state map[string]any
	if json.Unmarshal(raw, &state) != nil {
		os.Exit(1)
	}
	row, _ := json.Marshal(map[string]any{"args": args, "env": map[string]string{"OMPCODE": os.Getenv("OMPCODE"), "PI_SESSION_ID": os.Getenv("PI_SESSION_ID")}})
	f, err := os.OpenFile(os.Getenv("KATA_NATIVE_PLAN_CALLS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec // G703: recorder path is set by the fixture under t.TempDir.
	if err != nil {
		os.Exit(1)
	}
	_, _ = f.Write(append(row, '\n'))
	_ = f.Close()
	output := func(value any) { data, _ := json.Marshal(value); fmt.Println(string(data)) }
	if len(args) == 0 {
		os.Exit(1)
	}
	if args[0] == "plugin" {
		output(map[string]any{"marketplace": []any{}})
		os.Exit(0)
	}
	if args[0] == "list" && state["local"] == true {
		fmt.Fprintln(os.Stderr, "project_not_initialized")
		os.Exit(1)
	}
	rev := 1
	if n, ok := state["revision"].(float64); ok {
		rev = int(n)
	}
	meta := map[string]any{"todo.phase": "Tasks", "todo.status": "in_progress"}
	if m, ok := state["metadata"].(map[string]any); ok {
		meta = m
	}
	child := map[string]any{"short_id": "child", "uid": "child-uid", "title": "Current task", "status": "open", "revision": rev, "owner": "actor", "metadata": meta}
	if state["closed"] == true {
		child["status"] = "closed"
	}
	parent := map[string]any{"short_id": "parent", "status": "open", "revision": 1, "metadata": map[string]any{}}
	switch args[0] {
	case "list":
		output(map[string]any{"issues": []any{}})
	case "health":
		output(map[string]any{"api_schema_version": apiVersionPlanLayer})
	case "status":
		output(map[string]any{"actor": "actor"})
	case "show":
		if strings.HasSuffix(args[1], "#child") {
			output(map[string]any{"issue": child})
		} else {
			output(map[string]any{"issue": parent, "children": []any{child}})
		}
	case "close":
		state["closed"] = true
		state["revision"] = rev + 1
		output(map[string]any{})
	case "meta":
		if len(args) > 4 && args[1] == "set" {
			var value any
			if strings.Contains(strings.Join(args, " "), "--json-value") {
				_ = json.Unmarshal([]byte(args[4]), &value)
			} else {
				value = args[4]
			}
			meta[args[3]] = value
		}
		state["metadata"] = meta
		state["revision"] = rev + 1
		output(map[string]any{})
	default:
		fmt.Fprintln(os.Stderr, "unexpected fixture command", args)
		os.Exit(1)
	}
	data, _ := json.Marshal(state)
	_ = os.WriteFile(statePath, data, 0600) //nolint:gosec // G703: subprocess state path is set by the fixture under t.TempDir.
	os.Exit(0)
}

func ompPlanCommand(t *testing.T, fixture string, local bool) *exec.Cmd {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state.json")
	data, _ := json.Marshal(map[string]any{"local": local})
	require.NoError(t, os.WriteFile(state, data, 0600))
	bin := filepath.Join(root, "omp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.Link(os.Args[0], bin); err != nil {
		data, e := os.ReadFile(os.Args[0]) //nolint:gosec // G703: copy the currently running test binary into t.TempDir.
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(bin, data, 0700)) //nolint:gosec // G306: copied test binary must be executable.
	}
	generated, err := generateOmpAgentHooks(ompAgentHookMetadata{Format: "kata-omp", Version: 1, Scope: "user", Executable: os.Args[0], Plan: true, Contract: fixture == "omp-plan-compose.mjs", Attention: fixture == "omp-plan-session.mjs"})
	require.NoError(t, err)
	file := filepath.Join(root, "kata.mjs")
	require.NoError(t, os.WriteFile(file, generated, 0600))
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	command := exec.Command(node, filepath.Join("testdata/agent-hooks", fixture), file) //nolint:gosec // G204: fixture names are fixed by the Go tests and files are repository-owned.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KATA_") && !strings.HasPrefix(entry, "PATH=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "KATA_NATIVE_PLAN_FIXTURE=1", "KATA_NATIVE_PLAN_STATE="+state, "KATA_NATIVE_PLAN_CALLS="+filepath.Join(root, "calls.ndjson"))
	return command
}
func TestOmpPlanGeneratedFileParses(t *testing.T) {
	cmd := ompPlanCommand(t, "omp-plan.mjs", true)
	file := cmd.Args[2]
	for _, tc := range []struct{ name, source string }{{"generated", ""}, {"core", agenthookjs.PlanCore}, {"host", agenthookjs.PlanOmp}} {
		t.Run(tc.name, func(t *testing.T) {
			target := file
			if tc.source != "" {
				target = filepath.Join(t.TempDir(), tc.name+".mjs")
				require.NoError(t, os.WriteFile(target, []byte(tc.source), 0600))
			} else {
				generated, err := os.ReadFile(target) //nolint:gosec // G304: generated adapter is under t.TempDir.
				require.NoError(t, err)
				require.Contains(t, string(generated), `"minDaemonAPI":"`+apiVersionPlanLayer+`"`)
			}
			out, err := exec.Command(cmd.Path, "--check", target).CombinedOutput() //nolint:gosec // G204: runs Node's syntax check on a generated file under t.TempDir.
			require.NoError(t, err, "%s", out)
		})
	}
}
func TestOmpPlanTodoOps(t *testing.T) { runPlanNode(t, ompPlanCommand(t, "omp-plan.mjs", true)) }
func TestOmpPlanComposition(t *testing.T) {
	runPlanNode(t, ompPlanCommand(t, "omp-plan-compose.mjs", false))
}
func TestPlanSubprocessMarkers(t *testing.T) {
	runPlanNode(t, ompPlanCommand(t, "omp-plan-markers.mjs", false))
}
func TestOmpPlanGenerationReplacement(t *testing.T) {
	runPlanNode(t, planNode(t, "omp-plan-properties.mjs", "generation"))
}
func TestOmpPlanArmingProperty(t *testing.T) {
	exchange := planPropertyFixture(t, "omp-plan-properties.mjs", "arming")
	hegel.Test(t, func(ht *hegel.T) {
		input := map[string]any{"own": hegel.Draw(ht, hegel.Booleans()), "invoke": hegel.Draw(ht, hegel.Booleans()), "handler": hegel.Draw(ht, hegel.SampledFrom([]string{"input", "tool_call", "session_stop", "todo"})), "disarm": hegel.Draw(ht, hegel.Booleans())}
		var got struct {
			Possible bool `json:"possible"`
			Allowed  bool `json:"allowed"`
		}
		exchange(ht, input, &got)
		require.Equal(ompPropertyT{ht}, input["own"] == true && input["invoke"] == false && input["handler"] != "tool_call", got.Possible)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestPlanSubprocessMarkersProperty(t *testing.T) {
	exchange := planPropertyFixture(t, "omp-plan-properties.mjs", "markers")
	hegel.Test(t, func(ht *hegel.T) {
		session, inherited := hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.Text())
		var got struct {
			Count  int    `json:"count"`
			Marker string `json:"marker"`
		}
		exchange(ht, map[string]any{"session": session, "inherited": inherited}, &got)
		require.Positive(ompPropertyT{ht}, got.Count)
		require.Equal(ompPropertyT{ht}, session, got.Marker)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestOmpPlanGenerationProperty(t *testing.T) {
	exchange := planPropertyFixture(t, "omp-plan-properties.mjs", "generation")
	hegel.Test(t, func(ht *hegel.T) {
		// OMP session ids are portable opaque identifiers. Names and plan text keep
		// the complete Unicode draw domain, including empty/control text.
		oldID := fmt.Sprintf("session-%x", sha256.Sum256([]byte(hegel.Draw(ht, hegel.Text()))))[:32]
		newID := fmt.Sprintf("session-%x", sha256.Sum256([]byte(hegel.Draw(ht, hegel.Text()))))[:32]
		var got struct {
			Unchanged bool `json:"unchanged"`
		}
		exchange(ht, map[string]any{"oldID": oldID, "newID": newID, "oldTitle": hegel.Draw(ht, hegel.Text()), "newTitle": hegel.Draw(ht, hegel.Text()), "disarm": hegel.Draw(ht, hegel.Booleans())}, &got)
		require.True(ompPropertyT{ht}, got.Unchanged)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestOmpPlanSessionLifecycleFencesInflight(t *testing.T) {
	runPlanNode(t, ompPlanCommand(t, "omp-plan-session.mjs", false))
}
