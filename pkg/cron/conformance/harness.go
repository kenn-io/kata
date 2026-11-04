package conformance

import "testing"

// Run checks a Go adapter against the same policy-specific suite as the offline
// CLI. Expose scheduling decisions through Adapter; this never drives processes.
func Run(t *testing.T, a Adapter) {
	t.Helper()
	suite, err := Suite(t.Context(), a.Declare())
	if err != nil {
		t.Fatal(err)
	}
	answers := []Answer{}
	selected := []Scenario{}
	for _, s := range suite.Scenarios {
		t.Run(s.Name, func(t *testing.T) {
			selected = append(selected, s)
			answer, err := Execute(t.Context(), a, s)
			if err != nil {
				t.Fatal(err)
			}
			answers = append(answers, answer)
		})
	}
	suite.Scenarios = selected
	report, err := checkSuite(t.Context(), suite, answers)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range report.Failed {
		t.Errorf("%s: %s", failure.Name, failure.Diff)
	}
}
