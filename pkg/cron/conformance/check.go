package conformance

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

const maxPolicyBoundaryCandidates = 100001

// Execute feeds actual previous decisions through a scenario. The caller must
// configure the adapter to the declaration used to materialize that scenario.
func Execute(ctx context.Context, a Adapter, s Scenario) (Answer, error) {
	answer := Answer{Name: s.Name, Outputs: []jsontext.Value{}}
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
			out, err := a.Plan(ctx, in)
			if err != nil {
				return answer, err
			}
			previousPlan = &out
			output = out
		} else if step.Settle != nil {
			out, err := a.Settle(ctx, *step.Settle)
			if err != nil {
				return answer, err
			}
			previousSettle = &out
			output = out
		}
		raw, err := json.Marshal(output)
		if err != nil {
			return answer, err
		}
		answer.Outputs = append(answer.Outputs, raw)
	}
	return answer, nil
}

// Suite materializes the frozen scenarios for a declared policy and adds
// boundary inputs for enabled policy settings. Default policy retains the
// frozen outputs; all policies use the same Go and CLI checker. A generated
// count boundary is limited to 100,001 candidates so large declarations return
// an error instead of allocating an unbounded scenario.
func Suite(ctx context.Context, d Declaration) (FixtureSuite, error) {
	suite := FixtureSuite{Schema: Schema, Declaration: d}
	if err := d.Validate(); err != nil {
		return suite, err
	}
	scenarios, err := Scenarios()
	if err != nil {
		return suite, err
	}
	baseline := Reference{}.Declare()
	if d != baseline {
		boundaries, err := policyBoundaryScenarios(d)
		if err != nil {
			return suite, err
		}
		scenarios = append(scenarios, boundaries...)
		first := 0
		if samePolicy(d, baseline) {
			first = len(scenarios) - len(boundaries)
		}
		for i := first; i < len(scenarios); i++ {
			s := scenarios[i]
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

// samePolicy ignores the adapter identity because names do not change decisions.
func samePolicy(a, b Declaration) bool {
	a.Name = ""
	b.Name = ""
	return a == b
}

func policyBoundaryScenarios(d Declaration) ([]Scenario, error) {
	started := time.Date(2030, time.January, 4, 0, 0, 0, 0, time.UTC)
	dependency := SettleInput{
		Steps: []WorkflowStep{{Key: "a"}, {Key: "b", After: []string{"a"}}},
		Outcomes: map[string][]Attempt{
			"a": {{StartedAt: started, EndedAt: started.Add(time.Second), Status: "succeeded"}},
			"b": {{StartedAt: started.Add(time.Second), EndedAt: started.Add(2 * time.Second), Status: "succeeded"}},
		},
		StartedAt: started,
		Now:       started.Add(3 * time.Second),
	}
	cancelledAt := started.Add(2 * time.Second)
	mixedOutcome := SettleInput{
		Steps: []WorkflowStep{{Key: "a"}, {Key: "b"}},
		Outcomes: map[string][]Attempt{
			"a": {{StartedAt: started, EndedAt: started.Add(time.Second), Status: "failed"}},
			"b": {{StartedAt: started, Status: "running"}},
		},
		StartedAt:            started,
		Now:                  started.Add(3 * time.Second),
		CancelledAt:          &cancelledAt,
		StoppedWithCertainty: true,
	}
	scenarios := []Scenario{
		{Name: "retries/policy-successful-dependent-step", Steps: []Decision{{Settle: &dependency}}},
		{Name: "retries/policy-cancellation-over-failure", Steps: []Decision{{Settle: &mixedOutcome}}},
	}
	uid := "01ARZ3NDEKTSV4RRFFQ69G5FAB"
	if d.ToleranceSeconds > 0 {
		now := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
		boundary := now.Add(-time.Second - time.Duration(d.ToleranceSeconds)*time.Second)
		created := boundary.Add(-2 * time.Second)
		in := PlanInput{
			Adapter: "example-adapter",
			Job: Job{
				CreatedAt:          created,
				DefinitionEventUID: uid,
				Definition:         jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":1},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"skip","grace_seconds":1}`),
			},
			InstalledAt: created,
			Now:         now,
			Checkpoint: &Checkpoint{
				Through:            now,
				DefinitionEventUID: uid,
				Pending:            []time.Time{boundary.Add(-time.Second), boundary},
			},
		}
		scenarios = append(scenarios, Scenario{
			Name:  "schedule/policy-tolerance-boundary",
			Steps: []Decision{{Plan: &in}},
		})
	}
	if d.CatchupLimit > 0 {
		if d.CatchupLimit >= maxPolicyBoundaryCandidates {
			return nil, fmt.Errorf("policy boundary exceeds %d candidates for catchup_limit %d", maxPolicyBoundaryCandidates, d.CatchupLimit)
		}
		count := d.CatchupLimit + 1
		now := time.Date(2030, time.January, 2, 0, 0, 0, 0, time.UTC)
		pending := make([]time.Time, count)
		at := now.Add(time.Duration(count) * time.Second)
		for i := count - 1; i >= 0; i-- {
			pending[i] = at
			at = at.Add(-time.Second)
		}
		created := at.Add(-time.Second)
		in := PlanInput{
			Adapter: "example-adapter",
			Job: Job{
				CreatedAt:          created,
				DefinitionEventUID: uid,
				Definition:         jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":1},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"skip","grace_seconds":172800}`),
			},
			InstalledAt: created,
			Now:         now,
			Checkpoint: &Checkpoint{
				Through:            pending[len(pending)-1],
				DefinitionEventUID: uid,
				Pending:            pending,
			},
		}
		scenarios = append(scenarios, Scenario{
			Name:  "catchup/policy-count-boundary",
			Steps: []Decision{{Plan: &in}},
		})
	}
	if d.CatchupWindowSeconds > 0 {
		now := time.Date(2030, time.January, 3, 0, 0, 0, 0, time.UTC)
		boundary := now.Add(-time.Duration(d.CatchupWindowSeconds) * time.Second)
		created := boundary.Add(-2 * time.Second)
		in := PlanInput{
			Adapter: "example-adapter",
			Job: Job{
				CreatedAt:          created,
				DefinitionEventUID: uid,
				Definition:         jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":1},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}`),
			},
			InstalledAt: created,
			Now:         now,
			Checkpoint: &Checkpoint{
				Through:            now,
				DefinitionEventUID: uid,
				Pending:            []time.Time{boundary.Add(-time.Second), boundary},
			},
		}
		scenarios = append(scenarios, Scenario{
			Name:  "catchup/policy-age-window-boundary",
			Steps: []Decision{{Plan: &in}},
		})
	}
	if d.FailurePolicy == "stop" {
		prerequisiteInput := SettleInput{
			Steps: []WorkflowStep{{Key: "a"}, {Key: "b", After: []string{"a"}}},
			Outcomes: map[string][]Attempt{
				"a": {{StartedAt: started, EndedAt: started, Status: "succeeded"}},
				"b": {{StartedAt: started, EndedAt: started, Status: "failed"}},
			},
			StartedAt:            started,
			Now:                  started.Add(time.Second),
			StoppedWithCertainty: true,
		}
		scenarios = append(scenarios, Scenario{
			Name:  "retries/policy-stop-preserves-zero-duration-prerequisite",
			Steps: []Decision{{Settle: &prerequisiteInput}},
		})
		for _, reversed := range []bool{false, true} {
			steps := []WorkflowStep{{Key: "a"}, {Key: "b"}}
			name := "retries/policy-stop-preserves-started-independent-forward"
			if reversed {
				steps = []WorkflowStep{{Key: "b"}, {Key: "a"}}
				name = "retries/policy-stop-preserves-started-independent-reversed"
			}
			input := SettleInput{
				Steps: steps,
				Outcomes: map[string][]Attempt{
					"a": {{StartedAt: started, EndedAt: started.Add(2 * time.Second), Status: "failed"}},
					"b": {{StartedAt: started.Add(time.Second), EndedAt: started.Add(3 * time.Second), Status: "succeeded"}},
				},
				StartedAt:            started,
				Now:                  started.Add(4 * time.Second),
				StoppedWithCertainty: true,
			}
			scenarios = append(scenarios, Scenario{
				Name:  name,
				Steps: []Decision{{Settle: &input}},
			})
		}
	}
	return scenarios, nil
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
				if err := decodeOutput(raw, scenario.Steps[i], &got); err != nil {
					diff = fmt.Sprintf("decision %d: invalid JSON: %v", i+1, err)
					break
				}
				if err := decodeOutput(scenario.Steps[i].Expect, scenario.Steps[i], &want); err != nil {
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
		raw, err := json.Marshal(bundle{Schema: Schema, Scenarios: scenarios}, jsontext.WithIndent("  "), json.Deterministic(true))
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
func decodeOutput(raw []byte, decision Decision, value *any) error {
	var normalized any
	if decision.Plan != nil {
		var out PlanOutput
		if err := json.Unmarshal(raw, &out, json.RejectUnknownMembers(true)); err != nil {
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
