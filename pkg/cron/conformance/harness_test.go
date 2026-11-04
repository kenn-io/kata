package conformance

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/stretchr/testify/require"
	"os"
	"os/exec"
	"testing"
)

func TestHarnessSelection(t *testing.T) {
	if os.Getenv("KATA_CONFORMANCE_FILTER_CHILD") == "1" {
		Run(t, Reference{})
		return
	}
	for _, test := range []struct {
		pattern string
		want    string
	}{
		{pattern: "^TestHarnessSelection$/^does-not-exist$", want: "testing: warning: no tests to run"},
		{pattern: "^TestHarnessSelection$/^schedule$/^interval-grid$", want: "--- PASS: TestHarnessSelection/schedule/interval-grid"},
	} {
		//nolint:gosec // Runs the current test binary with constant test-name filters.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run="+test.pattern, "-test.v")
		cmd.Env = append(os.Environ(), "KATA_CONFORMANCE_FILTER_CHILD=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		require.Contains(t, string(output), test.want)
	}
}

type harnessFailureAdapter struct {
	Reference
	failPlanJSON string
}

func (a *harnessFailureAdapter) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	raw, err := json.Marshal(in, json.Deterministic(true))
	if err != nil {
		return PlanOutput{}, err
	}
	if string(raw) == a.failPlanJSON {
		return PlanOutput{}, errors.New("planned test failure")
	}
	return a.Reference.Plan(ctx, in)
}

func TestHarnessReportsFailuresOnScenarioSubtests(t *testing.T) {
	if os.Getenv("KATA_CONFORMANCE_REPORT_FAILURE_CHILD") == "1" {
		Run(t, &harnessFailureAdapter{failPlanJSON: os.Getenv("KATA_CONFORMANCE_REPORT_FAILURE_PLAN")})
		return
	}

	scenarios, err := Scenarios()
	require.NoError(t, err)
	planCounts := map[string]int{}
	for _, scenario := range scenarios {
		for _, step := range scenario.Steps {
			if step.Plan == nil {
				continue
			}
			raw, err := json.Marshal(step.Plan, json.Deterministic(true))
			require.NoError(t, err)
			planCounts[string(raw)]++
		}
	}
	var targetScenario, targetPlan string
	for _, scenario := range scenarios {
		for _, step := range scenario.Steps {
			if step.Plan == nil {
				continue
			}
			raw, err := json.Marshal(step.Plan, json.Deterministic(true))
			require.NoError(t, err)
			if planCounts[string(raw)] == 1 {
				targetScenario, targetPlan = scenario.Name, string(raw)
				break
			}
		}
		if targetPlan != "" {
			break
		}
	}
	require.NotEmpty(t, targetScenario, "expected a fixture with a unique plan input")
	require.NotEmpty(t, targetPlan)

	//nolint:gosec // Runs this test binary with a constant test-name filter.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHarnessReportsFailuresOnScenarioSubtests$", "-test.v")
	cmd.Env = append(os.Environ(), "KATA_CONFORMANCE_REPORT_FAILURE_CHILD=1", "KATA_CONFORMANCE_REPORT_FAILURE_PLAN="+targetPlan)
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "--- FAIL: TestHarnessReportsFailuresOnScenarioSubtests/"+targetScenario)
	require.Contains(t, string(output), "planned test failure")
	require.NotContains(t, string(output), "missing answer")
}
