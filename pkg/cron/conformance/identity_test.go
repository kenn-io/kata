package conformance_test

import (
	"encoding/json/v2"
	"slices"
	"testing"

	"go.kenn.io/kata/pkg/cron/conformance"
)

// The documented offline flow exports the default vectors before the adapter
// supplies its own implementation name. Build answers from those exported
// literals, not from Suite with the checker's declaration: doing the latter
// would silently give the adapter the extra scenario this regression detects.
func TestDefaultExportAcceptsAdapterIdentity(t *testing.T) {
	baseline := conformance.Reference{}.Declare()
	files, err := conformance.ExportFiles(t.Context(), baseline)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if name != "manifest.json" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	answers := []conformance.Answer{}
	for _, name := range names {
		var exported struct {
			Scenarios []conformance.Scenario `json:"scenarios"`
		}
		if err := json.Unmarshal(files[name], &exported); err != nil {
			t.Fatal(err)
		}
		for _, scenario := range exported.Scenarios {
			answer := conformance.Answer{Name: scenario.Name}
			for _, decision := range scenario.Steps {
				answer.Outputs = append(answer.Outputs, decision.Expect)
			}
			answers = append(answers, answer)
		}
	}
	declaration := baseline
	declaration.Name = "example-adapter"
	declaration.ForbidDeferral = "queue"
	report, err := conformance.Check(t.Context(), declaration, answers)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed != len(answers) || len(report.Failed) != 0 {
		t.Fatalf("implementation identity/default queue changed suite: %+v", report)
	}
}
