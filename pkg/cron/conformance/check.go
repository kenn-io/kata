package conformance

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Execute drives a sequence with the adapter's actual checkpoint and occupancy.
func Execute(ctx context.Context, a Adapter, s Scenario) (Answer, error) {
	answer := Answer{Name: s.Name, Outputs: []jsontext.Value{}}
	d := a.Declare()
	if err := d.Validate(); err != nil {
		return answer, err
	}
	var recorder DropRecorder
	if d.DropRecords {
		var ok bool
		recorder, ok = a.(DropRecorder)
		if !ok {
			return answer, fmt.Errorf("drop_records requires DropRecorder")
		}
	}
	var plan *PlanOutput
	var settle *SettleOutput
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
				if plan == nil {
					return answer, fmt.Errorf("missing previous checkpoint")
				}
				in.Checkpoint = &plan.Checkpoint
			}
			if step.UseOccupancy {
				if settle == nil {
					return answer, fmt.Errorf("missing previous occupancy")
				}
				in.ActiveRuns = 0
				if settle.Occupied {
					in.ActiveRuns = 1
				}
			}
			var out PlanOutput
			var drops []Drop
			var err error
			if recorder != nil {
				out, drops, err = recorder.PlanWithDrops(ctx, in)
			} else {
				out, err = a.Plan(ctx, in)
			}
			if err != nil {
				return answer, err
			}
			plan = &out
			output = out
			if recorder != nil {
				if err := validateDrops(drops); err != nil {
					return answer, err
				}
				output = recordedPlanOutput{PlanOutput: out, Drops: append([]Drop{}, drops...)}
			}
		} else {
			if step.UseCheckpoint || step.UseOccupancy {
				return answer, fmt.Errorf("sequence flags require a plan")
			}
			out, err := a.Settle(ctx, *step.Settle)
			if err != nil {
				return answer, err
			}
			settle = &out
			output = out
		}
		raw, err := json.Marshal(output, json.Deterministic(true))
		if err != nil {
			return answer, err
		}
		answer.Outputs = append(answer.Outputs, raw)
	}
	return answer, nil
}

func samePolicy(a, b Declaration) bool {
	a.Name = ""
	b.Name = ""
	if a.ForbidDeferral == "" {
		a.ForbidDeferral = "queue"
	}
	if b.ForbidDeferral == "" {
		b.ForbidDeferral = "queue"
	}
	return a == b
}

// Suite retains a bundle's own literal oracle whenever its policy matches.
// Other policies get reference-materialized answers and targeted boundaries.
func Suite(ctx context.Context, d Declaration) (FixtureSuite, error) { return NewSuite(ctx, Schema, d) }

// NewSuite selects exactly one supported fixture version. V1 preserves its
// frozen default vectors; v2 adds independently specified workflow decisions.
func NewSuite(ctx context.Context, schema string, d Declaration) (FixtureSuite, error) {
	suite := FixtureSuite{Schema: schema, Declaration: d}
	if schema != Schema && schema != SchemaV2 {
		return suite, fmt.Errorf("unsupported conformance schema %q", schema)
	}
	if err := ctx.Err(); err != nil {
		return suite, err
	}
	if err := d.Validate(); err != nil {
		return suite, err
	}
	bundles, err := readBundles(d.DropRecords)
	if err != nil {
		return suite, err
	}
	for _, b := range bundles {
		policy := Reference{}.Declare()
		if b.Declaration != nil {
			policy = *b.Declaration
		}
		if !samePolicy(d, policy) {
			if err := materialize(ctx, d, b.Scenarios); err != nil {
				return suite, err
			}
		}
		suite.Scenarios = append(suite.Scenarios, b.Scenarios...)
	}
	// Implementation identity and an explicit queue default do not change policy.
	if !samePolicy(d, (Reference{}).Declare()) {
		// Policy limits are valid independently of the kit's bounded test traces.
		// Check before CatchupLimit+1 so even the maximum int cannot overflow.
		if d.CatchupLimit > maxScan {
			return suite, fmt.Errorf("policy boundary resource limit exceeds %d candidates", maxScan+1)
		}
		boundaries := policyBoundaryScenarios(d)
		if err := materialize(ctx, d, boundaries); err != nil {
			return suite, err
		}
		suite.Scenarios = append(suite.Scenarios, boundaries...)
	}
	if schema == SchemaV2 {
		path := "testdata/adapter-v2/workflow.json"
		if d.FailurePolicy == "stop" {
			path = "testdata/adapter-v2/workflow-stop.json"
		}
		raw, err := fixtures.ReadFile(path)
		if err != nil {
			return suite, err
		}
		workflow, err := loadBundleVersion(raw, SchemaV2)
		if err != nil {
			return suite, err
		}
		// Other declaration members affect planning only. Both branch policies
		// have fixed workflow literals; never rematerialize their oracle.
		suite.Scenarios = append(suite.Scenarios, workflow.Scenarios...)
	}
	return suite, nil
}

func materialize(ctx context.Context, d Declaration, scenarios []Scenario) error {
	for i, s := range scenarios {
		answer, err := Execute(ctx, Reference{Policy: d}, s)
		if err != nil {
			return err
		}
		for j, raw := range answer.Outputs {
			scenarios[i].Steps[j].Expect = raw
		}
	}
	return nil
}

func policyBoundaryScenarios(d Declaration) []Scenario {
	scenarios := []Scenario{}
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	// These are already-enumerated pending values, so caps are tested without
	// depending on the reference's bounded scan or on a native batch limit.
	pendingCase := func(name, catchup string, created, now, through time.Time, pending []time.Time) {
		definition := fmt.Appendf(nil, `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":1},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":%q,"grace_seconds":172800}`, catchup)
		in := PlanInput{Adapter: "example-adapter", Job: Job{CreatedAt: created, DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Definition: definition}, InstalledAt: created, Now: now, Checkpoint: &Checkpoint{Through: through, DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Pending: pending}}
		scenarios = append(scenarios, Scenario{Name: name, Steps: []Decision{{Plan: &in}}})
	}
	if d.ToleranceSeconds > 0 {
		now := start.AddDate(0, 0, 1)
		boundary := now.Add(-time.Duration(1+d.ToleranceSeconds) * time.Second)
		pendingCase("schedule/policy-tolerance-boundary", "skip", start, now, now, []time.Time{boundary.Add(-time.Second), boundary})
		scenarios[len(scenarios)-1].Steps[0].Plan.Job.Definition = jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":1},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"skip","grace_seconds":1}`)
	}
	if d.CatchupLimit > 0 {
		now := start.AddDate(0, 0, 1)
		pending := make([]time.Time, d.CatchupLimit+1)
		for i := range pending {
			pending[i] = now.Add(time.Duration(i+1) * time.Second)
		}
		pendingCase("catchup/policy-count-boundary", "skip", now.Add(-time.Second), now, pending[len(pending)-1], pending)
	}
	if d.CatchupWindowSeconds > 0 {
		now := start.AddDate(0, 0, 2)
		boundary := now.Add(-time.Duration(d.CatchupWindowSeconds) * time.Second)
		pendingCase("catchup/policy-age-window-boundary", "all", boundary.Add(-2*time.Second), now, now, []time.Time{boundary.Add(-time.Second), boundary})
	}
	if d.FailurePolicy == "stop" {
		offers := map[string][]Attempt{"a": {{StartedAt: start, EndedAt: start, Status: "succeeded"}}, "b": {{StartedAt: start, EndedAt: start, Status: "failed"}}}
		in := SettleInput{Steps: []WorkflowStep{{Key: "a"}, {Key: "b", After: []string{"a"}}}, Outcomes: offers, StartedAt: start, Now: start.Add(time.Second), StoppedWithCertainty: true}
		scenarios = append(scenarios, Scenario{Name: "retries/policy-stop-preserves-zero-duration-prerequisite", Steps: []Decision{{Settle: &in}}})
		for _, reverse := range []bool{false, true} {
			steps := []WorkflowStep{{Key: "a"}, {Key: "b"}}
			order := "forward"
			if reverse {
				slices.Reverse(steps)
				order = "reversed"
			}
			in := SettleInput{Steps: steps, Outcomes: map[string][]Attempt{"a": {{StartedAt: start, EndedAt: start.Add(2 * time.Second), Status: "failed"}}, "b": {{StartedAt: start.Add(time.Second), EndedAt: start.Add(3 * time.Second), Status: "succeeded"}}}, StartedAt: start, Now: start.Add(4 * time.Second), StoppedWithCertainty: true}
			scenarios = append(scenarios, Scenario{Name: "retries/policy-stop-preserves-started-independent-" + order, Steps: []Decision{{Settle: &in}}})
		}
	}
	return scenarios
}

// Check requires exactly one answer for each policy-specific scenario.
func Check(ctx context.Context, d Declaration, answers []Answer) (Report, error) {
	suite, err := Suite(ctx, d)
	if err != nil {
		return Report{Schema: Schema, Failed: []Failure{}}, err
	}
	return suite.Check(ctx, answers)
}

// Check compares answers against this explicitly selected fixture suite.
func (suite FixtureSuite) Check(ctx context.Context, answers []Answer) (Report, error) {
	if suite.Schema != Schema && suite.Schema != SchemaV2 {
		return Report{}, fmt.Errorf("unsupported conformance schema %q", suite.Schema)
	}
	return checkSuite(ctx, suite, answers)
}

func checkSuite(ctx context.Context, suite FixtureSuite, answers []Answer) (Report, error) {
	report := Report{Schema: suite.Schema, Failed: []Failure{}}
	expected := map[string]Scenario{}
	seen := map[string]bool{}
	for _, s := range suite.Scenarios {
		expected[s.Name] = s
	}
	for _, a := range answers {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		diff := ""
		scenario, exists := expected[a.Name]
		switch {
		case seen[a.Name]:
			diff = "duplicate answer"
		case !exists:
			diff = "unknown scenario"
		default:
			if len(a.Outputs) != len(scenario.Steps) {
				diff = fmt.Sprintf("got %d decision outputs, want %d", len(a.Outputs), len(scenario.Steps))
			}
			// Validate every known decision, even after a value/count mismatch.
			for i := range min(len(a.Outputs), len(scenario.Steps)) {
				raw := a.Outputs[i]
				got, err := decodeOutput(raw, scenario.Steps[i], suite.Declaration)
				if err != nil {
					return report, fmt.Errorf("%s decision %d: malformed output: %w", a.Name, i+1, err)
				}
				want, err := decodeOutput(scenario.Steps[i].Expect, scenario.Steps[i], suite.Declaration)
				if err != nil {
					return report, err
				}
				if diff == "" && !reflect.DeepEqual(got, want) {
					diff = fmt.Sprintf("decision %d differs; expected %s; got %s", i+1, abbreviate(scenario.Steps[i].Expect), abbreviate(raw))
				}
			}
		}
		seen[a.Name] = true
		if diff != "" {
			report.Failed = append(report.Failed, Failure{Name: a.Name, Diff: diff})
		} else {
			report.Passed++
		}
	}
	for _, s := range suite.Scenarios {
		if !seen[s.Name] {
			report.Failed = append(report.Failed, Failure{Name: s.Name, Diff: "missing answer"})
		}
	}
	return report, nil
}

func abbreviate(raw []byte) string {
	if len(raw) > 300 {
		return string(raw[:300]) + "..."
	}
	return string(raw)
}

// requiredOutputFields distinguishes absence/null from valid zero values.
// Typed decoding below rejects unknown members and enforces the member types.
func requiredOutputFields(raw []byte, names ...string) (map[string]jsontext.Value, error) {
	// No output DTO permits null, including map values and array elements.
	// Token inspection preserves exact numeric types and distinguishes "null".
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := decoder.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if token.Kind() == jsontext.KindNull {
			return nil, fmt.Errorf("output requires non-null value at %s", decoder.StackPointer())
		}
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range names {
		_, ok := fields[name]
		if !ok {
			return nil, fmt.Errorf("output requires non-null %s", name)
		}
	}
	return fields, nil
}

func decodeSettleOutput(raw []byte) (SettleOutput, error) {
	var out SettleOutput
	if _, err := requiredOutputFields(raw, "attempts", "skipped", "status", "occupied"); err != nil {
		return out, err
	}
	err := json.Unmarshal(raw, &out, json.RejectUnknownMembers(true))
	return out, err
}

// Decode DTOs before comparing: integers stay exact and times use UTC instants.
func decodeOutput(raw []byte, step Decision, d Declaration) (any, error) {
	if step.Plan == nil {
		return decodeSettleOutput(raw)
	}
	out, drops, err := decodePlanOutput(raw, d)
	if err != nil {
		return nil, err
	}
	out.Checkpoint.Through = out.Checkpoint.Through.UTC()
	for i := range out.Launch {
		out.Launch[i] = out.Launch[i].UTC()
	}
	for i := range out.Checkpoint.Pending {
		out.Checkpoint.Pending[i] = out.Checkpoint.Pending[i].UTC()
	}
	if d.DropRecords {
		return recordedPlanOutput{PlanOutput: out, Drops: drops}, nil
	}
	return out, nil
}

// ExportFiles returns deterministic JSON bundles and a manifest without I/O.
func ExportFiles(ctx context.Context, d Declaration) (map[string][]byte, error) {
	suite, err := Suite(ctx, d)
	if err != nil {
		return nil, err
	}
	return suite.ExportFiles(ctx)
}

// ExportFiles serializes this suite, preserving its version and declaration.
func (suite FixtureSuite) ExportFiles(ctx context.Context) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if suite.Schema != Schema && suite.Schema != SchemaV2 {
		return nil, fmt.Errorf("unsupported conformance schema %q", suite.Schema)
	}
	d := suite.Declaration
	groups := map[string][]Scenario{}
	for _, s := range suite.Scenarios {
		area, _, _ := strings.Cut(s.Name, "/")
		groups[area] = append(groups[area], s)
	}
	files := map[string][]byte{}
	names := []string{}
	for area, scenarios := range groups {
		b := bundle{Schema: suite.Schema, Scenarios: scenarios}
		if d.DropRecords || suite.Schema == SchemaV2 {
			b.Declaration = &d
		}
		raw, err := json.Marshal(b, jsontext.WithIndent("  "), json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		name := area + ".json"
		files[name] = append(raw, '\n')
		names = append(names, name)
	}
	slices.Sort(names)
	manifest := struct {
		Schema      string      `json:"schema"`
		Declaration Declaration `json:"declaration"`
		Files       []string    `json:"files"`
	}{suite.Schema, d, names}
	raw, err := json.Marshal(manifest, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = append(raw, '\n')
	return files, nil
}
