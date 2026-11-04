package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"

	"go.kenn.io/kata/pkg/cron/conformance"
)

// These are malformed typed outputs, rather than valid decisions that differ
// from the oracle. Keep semantic mismatch tests expecting exit 1 separately.
func TestCronConformanceMalformedOutputExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		record   bool
		field    string
		value    jsontext.Value
		nested   string
	}{
		{"unknown plan member", "schedule/interval-first-after-anchor", false, "typo", jsontext.Value(`true`), ""},
		{"wrong launch type", "schedule/interval-first-after-anchor", false, "launch", jsontext.Value(`"invalid"`), ""},
		{"fractional attempt count", "retries/success", false, "attempts", jsontext.Value(`{"a":1.5}`), ""},
		{"null required drops", "drops/latest", true, "drops", jsontext.Value(`null`), ""},
		{"null excluded", "schedule/interval-first-after-anchor", false, "excluded", jsontext.Value(`null`), ""},
		{"null occupied", "retries/success", false, "occupied", jsontext.Value(`null`), ""},
		{"null pending", "schedule/interval-first-after-anchor", false, "pending", jsontext.Value(`null`), "checkpoint"},
		{"missing plan launch", "schedule/interval-first-after-anchor", false, "launch", nil, ""},
		{"missing plan checkpoint", "schedule/interval-first-after-anchor", false, "checkpoint", nil, ""},
		{"missing plan excluded", "schedule/interval-first-after-anchor", false, "excluded", nil, ""},
		{"missing checkpoint through", "schedule/interval-first-after-anchor", false, "through", nil, "checkpoint"},
		{"missing checkpoint definition_event_uid", "schedule/interval-first-after-anchor", false, "definition_event_uid", nil, "checkpoint"},
		{"missing checkpoint pending", "schedule/interval-first-after-anchor", false, "pending", nil, "checkpoint"},
		{"missing settle attempts", "retries/success", false, "attempts", nil, ""},
		{"missing settle skipped", "retries/success", false, "skipped", nil, ""},
		{"missing settle status", "retries/success", false, "status", nil, ""},
		{"missing settle occupied", "retries/success", false, "occupied", nil, ""},
		{"missing drops", "drops/latest", true, "drops", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declaration := conformance.Reference{}.Declare()
			declaration.DropRecords = tc.record
			lines := bytes.Split(bytes.TrimSpace(conformanceAnswers(t, declaration)), []byte{'\n'})
			answers := []conformance.Answer{}
			mutated := false
			for i := 1; i < len(lines); i++ {
				var answer conformance.Answer
				if err := json.Unmarshal(lines[i], &answer); err != nil {
					t.Fatal(err)
				}
				if answer.Name == tc.scenario {
					var fields map[string]jsontext.Value
					if err := json.Unmarshal(answer.Outputs[0], &fields); err != nil {
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
						nested, err := json.Marshal(target)
						if err != nil {
							t.Fatal(err)
						}
						fields[tc.nested] = nested
					}
					var err error
					answer.Outputs[0], err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					mutated = true
				}
				answers = append(answers, answer)
				var err error
				lines[i], err = json.Marshal(answer)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !mutated {
				t.Fatalf("fixture %q not found", tc.scenario)
			}
			if report, err := conformance.Check(t.Context(), declaration, answers); err == nil {
				t.Errorf("malformed output became a comparison result: %+v", report)
			}
			_, err := runConformance(t, t.Context(), bytes.Join(lines, []byte{'\n'}), "check", "--answers", "-")
			var cli *cliError
			if !errors.As(err, &cli) || cli.ExitCode != ExitValidation {
				t.Fatalf("want validation exit 3, got %v", err)
			}
		})
	}
}
