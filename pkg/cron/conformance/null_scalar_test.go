package conformance

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

// The output contract has no nullable scalar values. JSON null cannot stand in
// for a zero integer count, empty skipped key, or zero occurrence instant.
func TestOutputNullScalarsAreMalformed(t *testing.T) {
	scenarios, err := Scenarios()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, scenario, field string
		value                 jsontext.Value
	}{
		{"attempt map value", "retries/failed-dependency", "attempts", jsontext.Value(`{"a":1,"b":null}`)},
		{"skipped key", "retries/failed-dependency", "skipped", jsontext.Value(`[null]`)},
		{"launch instant", "schedule/interval-first-after-anchor", "launch", jsontext.Value(`[null]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scenario Scenario
			for _, candidate := range scenarios {
				if candidate.Name == tc.scenario {
					scenario = candidate
					break
				}
			}
			if scenario.Name == "" {
				t.Fatal("missing fixture")
			}
			var fields map[string]jsontext.Value
			if err := json.Unmarshal(scenario.Steps[0].Expect, &fields); err != nil {
				t.Fatal(err)
			}
			fields[tc.field] = tc.value
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			report, err := (FixtureSuite{Schema: Schema, Declaration: Reference{}.Declare(), Scenarios: []Scenario{scenario}}).Check(t.Context(), []Answer{{Name: scenario.Name, Outputs: []jsontext.Value{raw}}})
			if err == nil {
				t.Fatalf("null scalar treated as answer data: %+v", report)
			}
			if tc.field == "attempts" && !strings.Contains(err.Error(), "/attempts/b") {
				t.Fatalf("missing step path: %v", err)
			}
		})
	}
}
