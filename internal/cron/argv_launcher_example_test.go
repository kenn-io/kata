package cron_test

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"go.kenn.io/kata/pkg/client/generated"
)

// exampleAdapter demonstrates only argv execution; command is recognized for
// preflight, but its execution is outside this isolated process example.
type exampleAdapter struct {
	supportsArgv bool
	dir          string
	admissions   int
	launches     int
}

type exampleOutcome struct {
	status  string
	message string
	output  []byte
	err     error
}

func (a *exampleAdapter) run(ctx context.Context, workflow generated.CronWorkflowDefinitionResponse) exampleOutcome {
	for _, step := range workflow.Steps {
		if step.Kind != "command" && (step.Kind != "argv" || !a.supportsArgv) {
			return exampleOutcome{status: "failed", message: "reason: unsupported_step\nstep: " + step.Key}
		}
	}
	a.admissions++ // Stand-in for admission; no daemon or scheduler is involved.
	if len(workflow.Steps) != 1 {
		return exampleOutcome{status: "failed", err: errors.New("example executes one argv step only")}
	}
	step := workflow.Steps[0]
	if step.Kind != "argv" || len(step.Argv) == 0 {
		return exampleOutcome{status: "failed", err: errors.New("step outside argv example")}
	}
	argv := step.Argv
	a.launches++
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // The fixture intentionally supplies the executable.
	cmd.Dir = a.dir
	cmd.Env = []string{"KATA_ARGV_EXAMPLE_CHILD=1"}
	if root := os.Getenv("SystemRoot"); root != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+root)
	}
	output, err := cmd.Output()
	if err != nil {
		return exampleOutcome{status: "failed", err: err}
	}
	return exampleOutcome{status: "succeeded", output: output}
}
