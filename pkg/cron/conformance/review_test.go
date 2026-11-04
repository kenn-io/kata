package conformance

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

// Every known answer's outputs require typed validation, even when duplicate
// classification already establishes a comparison failure.
func TestKnownDuplicateOutputValidation(t *testing.T) {
	suite, err := Suite(t.Context(), Reference{}.Declare())
	if err != nil {
		t.Fatal(err)
	}
	scenario := suite.Scenarios[0]
	good, err := Execute(t.Context(), Reference{}, scenario)
	if err != nil {
		t.Fatal(err)
	}
	suite.Scenarios = []Scenario{scenario}
	report, err := suite.Check(t.Context(), []Answer{good, good})
	if err != nil || len(report.Failed) != 1 || report.Failed[0].Diff != "duplicate answer" {
		t.Fatalf("well-formed duplicate: %+v %v", report, err)
	}
	bad := Answer{Name: scenario.Name, Outputs: []jsontext.Value{jsontext.Value(`{"typo":true}`)}}
	report, err = suite.Check(t.Context(), []Answer{good, bad})
	if err == nil {
		t.Fatalf("malformed duplicate became comparison result: %+v", report)
	}
}

// A typed Unicode mismatch must remain a JSON-serializable report. This raw
// excluded-first output reproduces a multi-byte rune crossing byte 300.
func TestUnicodeMismatchReport(t *testing.T) {
	suite, err := Suite(t.Context(), Reference{}.Declare())
	if err != nil {
		t.Fatal(err)
	}
	scenario := suite.Scenarios[0]
	good, err := Execute(t.Context(), Reference{}, scenario)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(good.Outputs[0], &fields); err != nil {
		t.Fatal(err)
	}
	good.Outputs[0] = jsontext.Value(`{"excluded":"` + strings.Repeat("€", 101) + `","launch":` + string(fields["launch"]) + `,"checkpoint":` + string(fields["checkpoint"]) + `}`)
	suite.Scenarios = []Scenario{scenario}
	report, err := suite.Check(t.Context(), []Answer{good})
	if err != nil || len(report.Failed) != 1 {
		t.Fatalf("expected mismatch: %+v %v", report, err)
	}
	raw, err := json.Marshal(report)
	if err != nil || !jsontext.Value(raw).IsValid() {
		t.Fatalf("mismatch report cannot encode as JSON: %v, %q", err, raw)
	}
}
