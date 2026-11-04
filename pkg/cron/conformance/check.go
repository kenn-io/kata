package conformance

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Execute feeds actual previous decisions through a scenario. The caller must
// configure the adapter to the declaration used to materialize that scenario.
func Execute(ctx context.Context, a Adapter, s Scenario) (Answer, error) {
	answer := Answer{Name: s.Name, Outputs: []jsontext.Value{}}
	declaration := a.Declare()
	if err := declaration.Validate(); err != nil {
		return answer, err
	}
	var recorder DropRecorder
	if declaration.DropRecords {
		var ok bool
		recorder, ok = a.(DropRecorder)
		if !ok {
			return answer, fmt.Errorf("drop_records requires DropRecorder")
		}
	}
	var previousPlan *PlanOutput
	var previousSettle *SettleOutput
	for _, step := range s.Steps {
		if err := ctx.Err(); err != nil {
			return answer, err
		}
		if (step.Plan == nil) == (step.Settle == nil) {
			return answer, fmt.Errorf("decision requires exactly one input")
		}
		var output any
		if step.Plan != nil {
			in := *step.Plan
			if step.UseCheckpoint {
				if previousPlan == nil {
					return answer, fmt.Errorf("missing previous checkpoint")
				}
				in.Checkpoint = &previousPlan.Checkpoint
			}
			if step.UseOccupancy {
				if previousSettle == nil {
					return answer, fmt.Errorf("missing previous occupancy")
				}
				in.ActiveRuns = 0
				if previousSettle.Occupied {
					in.ActiveRuns = 1
				}
			}
			var out PlanOutput
			var drops []Drop
			var err error
			if declaration.DropRecords {
				out, drops, err = recorder.PlanWithDrops(ctx, in)
			} else {
				out, err = a.Plan(ctx, in)
			}
			if err != nil {
				return answer, err
			}
			previousPlan = &out
			output = out
			if declaration.DropRecords {
				if err := validateDrops(drops); err != nil {
					return answer, err
				}
				output = recordedPlanOutput{PlanOutput: out, Drops: append([]Drop{}, drops...)}
			}
		} else if step.Settle != nil {
			out, err := a.Settle(ctx, *step.Settle)
			if err != nil {
				return answer, err
			}
			previousSettle = &out
			output = out
		} else {
			return answer, fmt.Errorf("decision requires an input")
		}
		raw, err := json.Marshal(output, json.Deterministic(true))
		if err != nil {
			return answer, err
		}
		answer.Outputs = append(answer.Outputs, raw)
	}
	return answer, nil
}

// Suite materializes the fixed scenario inputs for a declared policy. Default
// policy retains the frozen expected outputs. Alternative policies use the
// reference's decisions; they share the same checker in Go and the CLI.
func Suite(ctx context.Context, d Declaration) (FixtureSuite, error) {
	suite := FixtureSuite{Schema: Schema, Declaration: d}
	if err := d.Validate(); err != nil {
		return suite, err
	}
	scenarios, err := Scenarios()
	if err != nil {
		return suite, err
	}
	if d.DropRecords {
		raw, err := fixtures.ReadFile("testdata/adapter-v1/drops.json")
		if err != nil {
			return suite, err
		}
		extra, err := Load(raw)
		if err != nil {
			return suite, err
		}
		scenarios = append(scenarios, extra...)
	}
	baseline := Reference{}.Declare()
	baseline.Name = d.Name
	if d != baseline {
		for i, s := range scenarios {
			answer, err := Execute(ctx, Reference{Policy: d}, s)
			if err != nil {
				return suite, err
			}
			for j, raw := range answer.Outputs {
				scenarios[i].Steps[j].Expect = raw
			}
		}
	}
	suite.Scenarios = scenarios
	return suite, nil
}

// Check requires exactly one answer per scenario, including ordered outputs for
// every decision. Comparison failures are data, while invalid declarations or
// broken fixtures are errors.
func Check(ctx context.Context, d Declaration, answers []Answer) (Report, error) {
	report := Report{Schema: Schema, Failed: []Failure{}}
	suite, err := Suite(ctx, d)
	if err != nil {
		return report, err
	}
	return checkSuite(ctx, suite, answers)
}

func checkSuite(ctx context.Context, suite FixtureSuite, answers []Answer) (Report, error) {
	report := Report{Schema: Schema, Failed: []Failure{}}
	expected := map[string]Scenario{}
	for _, s := range suite.Scenarios {
		expected[s.Name] = s
	}
	seen := map[string]bool{}
	for _, answer := range answers {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if seen[answer.Name] {
			report.Failed = append(report.Failed, Failure{answer.Name, "duplicate answer"})
			continue
		}
		seen[answer.Name] = true
		scenario, ok := expected[answer.Name]
		if !ok {
			report.Failed = append(report.Failed, Failure{answer.Name, "unknown scenario"})
			continue
		}
		diff := ""
		if len(answer.Outputs) != len(scenario.Steps) {
			diff = fmt.Sprintf("got %d decision outputs, want %d", len(answer.Outputs), len(scenario.Steps))
		} else {
			for i, raw := range answer.Outputs {
				var got, want any
				if err := decodeOutput(raw, scenario.Steps[i], suite.Declaration, &got); err != nil {
					diff = fmt.Sprintf("decision %d: invalid JSON: %v", i+1, err)
					break
				}
				if err := decodeOutput(scenario.Steps[i].Expect, scenario.Steps[i], suite.Declaration, &want); err != nil {
					return report, err
				}
				if !reflect.DeepEqual(got, want) {
					diff = fmt.Sprintf("decision %d differs; expected %s; got %s", i+1, abbreviate(scenario.Steps[i].Expect), abbreviate(raw))
					break
				}
			}
		}
		if diff != "" {
			report.Failed = append(report.Failed, Failure{answer.Name, diff})
		} else {
			report.Passed++
		}
	}
	for _, s := range suite.Scenarios {
		if !seen[s.Name] {
			report.Failed = append(report.Failed, Failure{s.Name, "missing answer"})
		}
	}
	return report, nil
}
func abbreviate(v []byte) string {
	if len(v) > 300 {
		return string(v[:300]) + "..."
	}
	return string(v)
}

// ExportFiles returns deterministic, caller-owned JSON bundles and a manifest.
// No filesystem writes or daemon access occur here.
func ExportFiles(ctx context.Context, d Declaration) (map[string][]byte, error) {
	suite, err := Suite(ctx, d)
	if err != nil {
		return nil, err
	}
	groups := map[string][]Scenario{}
	for _, s := range suite.Scenarios {
		area, _, _ := strings.Cut(s.Name, "/")
		groups[area] = append(groups[area], s)
	}
	files := map[string][]byte{}
	names := []string{}
	for area, scenarios := range groups {
		name := area + ".json"
		b := bundle{Schema: Schema, Scenarios: scenarios}
		if d.DropRecords {
			b.Declaration = &d
		}
		raw, err := json.Marshal(b, jsontext.WithIndent("  "), json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		files[name] = append(raw, '\n')
		names = append(names, name)
	}
	slices.Sort(names)
	manifest := struct {
		Schema      string      `json:"schema"`
		Declaration Declaration `json:"declaration"`
		Files       []string    `json:"files"`
	}{Schema, d, names}
	raw, err := json.Marshal(manifest, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = append(raw, '\n')
	return files, nil
}

// Decode output DTOs before comparison so integer counts cannot round through
// float64 and equivalent RFC3339 offsets compare as the same instant.
func decodeOutput(raw []byte, decision Decision, declaration Declaration, value *any) error {
	var normalized any
	if decision.Plan != nil {
		out, drops, err := decodePlanOutput(raw, declaration)
		if err != nil {
			return err
		}
		out.Checkpoint.Through = out.Checkpoint.Through.UTC()
		for i := range out.Launch {
			out.Launch[i] = out.Launch[i].UTC()
		}
		for i := range out.Checkpoint.Pending {
			out.Checkpoint.Pending[i] = out.Checkpoint.Pending[i].UTC()
		}
		normalized = out
		if declaration.DropRecords {
			normalized = recordedPlanOutput{PlanOutput: out, Drops: drops}
		}
	} else {
		var out SettleOutput
		if err := json.Unmarshal(raw, &out, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		normalized = out
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, value)
}
