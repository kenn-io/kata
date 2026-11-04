package conformance

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

// Validation must be independent of either an earlier mismatch or duplicate classification.
func TestKnownDuplicateOutputValidation(t *testing.T) {
	for _, kind := range []string{"earlier mismatch", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			suite, err := Suite(t.Context(), Reference{}.Declare())
			if err != nil {
				t.Fatal(err)
			}
			scenario := suite.Scenarios[0]
			if kind == "earlier mismatch" {
				for _, s := range suite.Scenarios {
					if s.Name == "overlap/same-clock-release" {
						scenario = s
						break
					}
				}
				if len(scenario.Steps) != 2 {
					t.Fatal("missing two-decision fixture")
				}
			}
			good, err := Execute(t.Context(), Reference{}, scenario)
			if err != nil {
				t.Fatal(err)
			}
			suite.Scenarios = []Scenario{scenario}
			answers := []Answer{good}
			if kind == "duplicate" {
				answers = append(answers, Answer{Name: scenario.Name, Outputs: []jsontext.Value{jsontext.Value(`{"typo":true}`)}})
			} else {
				for i, raw := range good.Outputs {
					var fields map[string]jsontext.Value
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatal(err)
					}
					if i == 0 {
						fields["excluded"] = jsontext.Value(`"disabled"`)
					} else {
						delete(fields, "excluded")
					}
					answers[0].Outputs[i], err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if report, err := suite.Check(t.Context(), answers); err == nil {
				t.Fatalf("malformed output became comparison result: %+v", report)
			}
		})
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
