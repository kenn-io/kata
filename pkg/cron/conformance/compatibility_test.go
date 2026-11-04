package conformance

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
)

// Load and a string-valued ForbidDeferral are existing public API contracts.
func TestPreservedPublicAPI(t *testing.T) {
	mode := strings.Join([]string{"drop", "after", "grace"}, "_")
	d := Declaration{Name: "example-adapter", FailurePolicy: "continue", ForbidDeferral: mode}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := fixtures.ReadFile("testdata/adapter-v1/schedule.json")
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := Load(raw)
	if err != nil || len(scenarios) == 0 {
		t.Fatalf("Load: %d scenarios, %v", len(scenarios), err)
	}
}

// Declaration bounds are product policy; enumeration/materialization budgets
// must not turn a valid large declaration into an invalid one.
func TestLargeDeclarationResourceBudget(t *testing.T) {
	d := Reference{}.Declare()
	d.CatchupLimit = int(^uint(0) >> 1)
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := Suite(t.Context(), d); err == nil || !strings.Contains(err.Error(), "resource") {
		t.Fatalf("expected bounded materialization resource error, got %v", err)
	}
}

// Time offsets identify the same schedule instant in the Go and CLI checker.
func TestEquivalentOutputInstants(t *testing.T) {
	suite, err := Suite(t.Context(), Reference{}.Declare())
	if err != nil {
		t.Fatal(err)
	}
	answers := []Answer{}
	for _, s := range suite.Scenarios {
		answer, err := Execute(t.Context(), Reference{}, s)
		if err != nil {
			t.Fatal(err)
		}
		for i, step := range s.Steps {
			if step.Plan == nil {
				continue
			}
			var out PlanOutput
			if err := json.Unmarshal(answer.Outputs[i], &out); err != nil {
				t.Fatal(err)
			}
			offset := time.FixedZone("example", 3600)
			out.Checkpoint.Through = out.Checkpoint.Through.In(offset)
			for j := range out.Launch {
				out.Launch[j] = out.Launch[j].In(offset)
			}
			for j := range out.Checkpoint.Pending {
				out.Checkpoint.Pending[j] = out.Checkpoint.Pending[j].In(offset)
			}
			answer.Outputs[i], err = json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
		}
		answers = append(answers, answer)
	}
	report, err := suite.Check(t.Context(), answers)
	if err != nil || len(report.Failed) > 0 {
		t.Fatalf("%+v %v", report, err)
	}
}
