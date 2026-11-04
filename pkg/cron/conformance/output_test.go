package conformance

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestMalformedOutputValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		field    string
		value    jsontext.Value
		nested   string
		path     string
	}{
		{"unknown plan member", "schedule/interval-first-after-anchor", "typo", jsontext.Value(`true`), "", ""},
		{"wrong launch type", "schedule/interval-first-after-anchor", "launch", jsontext.Value(`"invalid"`), "", ""},
		{"fractional attempt count", "retries/success", "attempts", jsontext.Value(`{"a":1.5}`), "", ""},
		{"null excluded", "schedule/interval-first-after-anchor", "excluded", jsontext.Value(`null`), "", ""},
		{"null occupied", "retries/success", "occupied", jsontext.Value(`null`), "", ""},
		{"null pending", "schedule/interval-first-after-anchor", "pending", jsontext.Value(`null`), "checkpoint", ""},
		{"missing plan launch", "schedule/interval-first-after-anchor", "launch", nil, "", ""},
		{"missing plan checkpoint", "schedule/interval-first-after-anchor", "checkpoint", nil, "", ""},
		{"missing plan excluded", "schedule/interval-first-after-anchor", "excluded", nil, "", ""},
		{"missing checkpoint through", "schedule/interval-first-after-anchor", "through", nil, "checkpoint", ""},
		{"missing checkpoint definition_event_uid", "schedule/interval-first-after-anchor", "definition_event_uid", nil, "checkpoint", ""},
		{"missing checkpoint pending", "schedule/interval-first-after-anchor", "pending", nil, "checkpoint", ""},
		{"missing settle attempts", "retries/success", "attempts", nil, "", ""},
		{"missing settle skipped", "retries/success", "skipped", nil, "", ""},
		{"missing settle status", "retries/success", "status", nil, "", ""},
		{"missing settle occupied", "retries/success", "occupied", nil, "", ""},
		{"attempt map value", "retries/failed-dependency", "attempts", jsontext.Value(`{"a":1,"b":null}`), "", "/attempts/b"},
		{"skipped key", "retries/failed-dependency", "skipped", jsontext.Value(`[null]`), "", ""},
		{"launch instant", "schedule/interval-first-after-anchor", "launch", jsontext.Value(`[null]`), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenarios, err := Scenarios()
			if err != nil {
				t.Fatal(err)
			}
			var scenario Scenario
			for _, s := range scenarios {
				if s.Name == tc.scenario {
					scenario = s
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
			target := fields
			if tc.nested != "" {
				target = map[string]jsontext.Value{}
				if err := json.Unmarshal(fields[tc.nested], &target); err != nil {
					t.Fatal(err)
				}
			}
			if tc.value == nil {
				delete(target, tc.field)
			} else {
				target[tc.field] = tc.value
			}
			if tc.nested != "" {
				raw, err := json.Marshal(target)
				if err != nil {
					t.Fatal(err)
				}
				fields[tc.nested] = raw
			}
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			report, err := (FixtureSuite{Schema: Schema, Declaration: Reference{}.Declare(), Scenarios: []Scenario{scenario}}).Check(t.Context(), []Answer{{Name: scenario.Name, Outputs: []jsontext.Value{raw}}})
			if err == nil {
				t.Fatalf("malformed output became a comparison result: %+v", report)
			}
			if tc.path != "" && !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("missing step path: %v", err)
			}
		})
	}
}

func TestNamedDropMismatches(t *testing.T) {
	raw, err := fixtures.ReadFile("testdata/adapter-v1/drops.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	var scenario Scenario
	for _, s := range b.Scenarios {
		if s.Name == "drops/latest" {
			scenario = s
			break
		}
	}
	if scenario.Name == "" {
		t.Fatal("missing fixture")
	}
	for _, tc := range []struct {
		name  string
		drops jsontext.Value
	}{
		{"reason", jsontext.Value(`[{"reason":"overlap_skipped","instants":["2030-01-01T01:00:00Z","2030-01-01T02:00:00Z"]}]`)},
		{"missing instant", jsontext.Value(`[{"reason":"catchup_missed","instants":["2030-01-01T01:00:00Z"]}]`)},
		{"extra group", jsontext.Value(`[{"reason":"catchup_missed","instants":["2030-01-01T01:00:00Z","2030-01-01T02:00:00Z"]},{"reason":"overlap_skipped","instants":["2030-01-01T03:00:00Z"]}]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fields map[string]jsontext.Value
			if err := json.Unmarshal(scenario.Steps[0].Expect, &fields); err != nil {
				t.Fatal(err)
			}
			fields["drops"] = tc.drops
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			report, err := (FixtureSuite{Schema: Schema, Declaration: *b.Declaration, Scenarios: []Scenario{scenario}}).Check(t.Context(), []Answer{{Name: scenario.Name, Outputs: []jsontext.Value{raw}}})
			if err != nil || len(report.Failed) != 1 || report.Failed[0].Name != scenario.Name {
				t.Fatalf("%+v %v", report, err)
			}
		})
	}
}
