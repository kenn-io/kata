package cron_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/pkg/client/generated"
)

// adapterWorkflow crosses the same JSON boundary as an adapter's workflow read.
// The argv kind is prospective until daemon admission is enabled.
func adapterWorkflow(t *testing.T, steps ...api.CronWorkflowStep) generated.CronWorkflowDefinitionResponse {
	t.Helper()
	wire, err := json.Marshal(api.CronWorkflowDefinition{Version: 1, Steps: steps})
	require.NoError(t, err)
	var workflow generated.CronWorkflowDefinitionResponse
	require.NoError(t, json.Unmarshal(wire, &workflow))
	return workflow
}

func TestAdapterArgvLiteralProcess(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	args := []string{"", "white space", "雪", `a; touch side-effect`, `$(touch side-effect)`, `*.txt`, `"quoted"`, "line one\nline two"}
	argv := append([]string{executable, "-test.run=^TestAdapterArgvChild$", "--"}, args...)
	workflow := adapterWorkflow(t, api.CronWorkflowStep{Key: "literal", Kind: "argv", Argv: argv})
	require.Equal(t, argv, workflow.Steps[0].Argv)

	dir := t.TempDir()
	adapter := exampleAdapter{supportsArgv: true, dir: dir}
	outcome := adapter.run(t.Context(), workflow)
	require.NoError(t, outcome.err)
	require.Equal(t, "succeeded", outcome.status)
	require.Equal(t, 1, adapter.admissions)
	require.Equal(t, 1, adapter.launches)
	var received []string
	require.NoError(t, json.Unmarshal(outcome.output, &received))
	require.Len(t, received, len(args))
	for i := range args {
		require.Equal(t, args[i], received[i], "argument %d", i)
	}
	_, err = os.Stat(filepath.Join(dir, "side-effect"))
	require.True(t, os.IsNotExist(err), "literal arguments must not create a file: %v", err)
}

func TestAdapterPreflightsAllSteps(t *testing.T) {
	workflow := adapterWorkflow(t,
		api.CronWorkflowStep{Key: "first", Kind: "command", Command: "echo first"},
		api.CronWorkflowStep{Key: "later", Kind: "argv", Argv: []string{"example-program"}},
	)
	adapter := exampleAdapter{dir: t.TempDir()}
	outcome := adapter.run(t.Context(), workflow)
	require.Equal(t, "failed", outcome.status)
	require.Equal(t, "reason: unsupported_step\nstep: later", outcome.message)
	require.Zero(t, adapter.admissions)
	require.Zero(t, adapter.launches)
}

func TestAdapterMissingProgramDoesNotRetry(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-program")
	workflow := adapterWorkflow(t, api.CronWorkflowStep{Key: "missing", Kind: "argv", Argv: []string{missing, `; touch side-effect`}})
	adapter := exampleAdapter{supportsArgv: true, dir: dir}
	outcome := adapter.run(t.Context(), workflow)
	require.Equal(t, "failed", outcome.status)
	require.Error(t, outcome.err)
	require.Equal(t, 1, adapter.launches)
	_, err := os.Stat(filepath.Join(dir, "side-effect"))
	require.True(t, os.IsNotExist(err))
}

func TestAdapterExplicitShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh is a Unix example")
	}
	workflow := adapterWorkflow(t, api.CronWorkflowStep{Key: "shell", Kind: "argv", Argv: []string{"/bin/sh", "-c", `printf '%s' "$1"`, "sh", "shell value"}})
	adapter := exampleAdapter{supportsArgv: true, dir: t.TempDir()}
	outcome := adapter.run(t.Context(), workflow)
	require.NoError(t, outcome.err)
	require.Equal(t, "succeeded", outcome.status)
	require.Equal(t, "shell value", string(outcome.output))
	require.Equal(t, 1, adapter.launches)
}

func TestAdapterArgvChild(_ *testing.T) {
	if os.Getenv("KATA_ARGV_EXAMPLE_CHILD") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			if err := json.NewEncoder(os.Stdout).Encode(os.Args[i+1:]); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}
