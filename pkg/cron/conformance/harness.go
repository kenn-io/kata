package conformance

import "testing"

// Run checks a Go adapter against the same policy suite as the offline CLI.
func Run(t *testing.T, a Adapter) {
	t.Helper()
	suite, err := Suite(t.Context(), a.Declare())
	if err != nil {
		t.Fatal(err)
	}
	RunSuite(t, a, suite)
}

// RunSuite checks an adapter using an explicitly selected shared fixture suite.
func RunSuite(t *testing.T, a Adapter, suite FixtureSuite) {
	t.Helper()
	if !samePolicy(a.Declare(), suite.Declaration) {
		t.Fatal("adapter declaration differs from fixture suite policy")
	}
	for _, s := range suite.Scenarios {
		t.Run(s.Name, func(t *testing.T) {
			answer, err := Execute(t.Context(), a, s)
			if err != nil {
				t.Fatal(err)
			}
			report, err := (FixtureSuite{Schema: suite.Schema, Declaration: suite.Declaration, Scenarios: []Scenario{s}}).Check(t.Context(), []Answer{answer})
			if err != nil {
				t.Fatal(err)
			}
			for _, failure := range report.Failed {
				t.Error(failure.Diff)
			}
		})
	}
}
