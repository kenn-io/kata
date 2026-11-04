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

type harnessFailureAdapter struct {
	Reference
	failPlanJSON string
}

func (a *harnessFailureAdapter) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	raw, err := json.Marshal(in, json.Deterministic(true))
	if err != nil {
		return PlanOutput{}, err
	}
	if a.failPlanJSON == "*" || string(raw) == a.failPlanJSON {
		return PlanOutput{}, errors.New("planned test failure")
	}
	return a.Reference.Plan(ctx, in)
}

func TestHarnessReportsFailuresOnScenarioSubtests(t *testing.T) {
	if child := os.Getenv("KATA_CONFORMANCE_REPORT_FAILURE_CHILD"); child != "" {
		var adapter Adapter = Reference{}
		if child == "unmatched" {
			adapter = &harnessFailureAdapter{failPlanJSON: "*"}
		}
		if child == "failure" {
			adapter = &harnessFailureAdapter{failPlanJSON: os.Getenv("KATA_CONFORMANCE_REPORT_FAILURE_PLAN")}
		}
		Run(t, adapter)
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

	for _, tc := range []struct {
		mode, pattern, want string
		failed              bool
	}{
		{"unmatched", "^TestHarnessReportsFailuresOnScenarioSubtests$/^does-not-exist$", "testing: warning: no tests to run", false},
		{"matched", "^TestHarnessReportsFailuresOnScenarioSubtests$/^schedule$/^interval-grid$", "--- PASS: TestHarnessReportsFailuresOnScenarioSubtests/schedule/interval-grid", false},
		{"failure", "^TestHarnessReportsFailuresOnScenarioSubtests$", "--- FAIL: TestHarnessReportsFailuresOnScenarioSubtests/" + targetScenario, true},
	} {
		//nolint:gosec // Runs the current test binary with constant test-name filters.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run="+tc.pattern, "-test.v")
		cmd.Env = append(os.Environ(), "KATA_CONFORMANCE_REPORT_FAILURE_CHILD="+tc.mode, "KATA_CONFORMANCE_REPORT_FAILURE_PLAN="+targetPlan)
		output, err := cmd.CombinedOutput()
		if tc.failed {
			require.Error(t, err)
			require.Contains(t, string(output), "planned test failure")
		} else {
			require.NoError(t, err, string(output))
		}
		require.Contains(t, string(output), tc.want)
		require.NotContains(t, string(output), "missing answer")
	}
}
