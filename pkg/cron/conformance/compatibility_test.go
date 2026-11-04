package conformance

import (
	"encoding/json/v2"
	"testing"
	"time"
)

func TestLargeDeclarationResourceBudget(t *testing.T) {
	d := Reference{}.Declare()
	d.CatchupLimit = int(^uint(0) >> 1)
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := Suite(t.Context(), d); err == nil || err.Error() != "suite cannot check catchup_limit >100000" {
		t.Fatalf("expected built-in suite limit diagnostic, got %v", err)
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
