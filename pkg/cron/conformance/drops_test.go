package conformance

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestDropsIndependentSwitches(t *testing.T) {
	for _, record := range []bool{false, true} {
		for _, mode := range []string{"queue", "drop_after_grace"} {
			t.Run(fmt.Sprintf("%v/%s", record, mode), func(t *testing.T) {
				d := Reference{}.Declare()
				d.DropRecords = record
				d.ForbidDeferral = mode
				ref := Reference{Policy: d}
				in := testPlan(t)
				in.Job.Definition = jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"latest"}`)
				in.ActiveRuns = 1
				in.Now = in.Now.Add(time.Second)
				out, drops, err := ref.PlanWithDrops(t.Context(), in)
				if err != nil {
					t.Fatal(err)
				}
				wantPending := []time.Time{in.Job.CreatedAt.Add(3 * time.Hour)}
				if mode == "drop_after_grace" {
					wantPending = []time.Time{}
				}
				if !reflect.DeepEqual(out.Checkpoint.Pending, wantPending) {
					t.Fatalf("pending %v", out.Checkpoint.Pending)
				}
				wantGroups := 0
				if record {
					wantGroups = 1
					if mode == "drop_after_grace" {
						wantGroups = 2
					}
				}
				if len(drops) != wantGroups {
					t.Fatalf("drops %+v", drops)
				}

			})
		}
	}
}

type recordingProbe struct {
	Reference
	planCalls, dropCalls int
}

func (p *recordingProbe) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	p.planCalls++
	return p.Reference.Plan(ctx, in)
}
func (p *recordingProbe) PlanWithDrops(ctx context.Context, in PlanInput) (PlanOutput, []Drop, error) {
	p.dropCalls++
	return p.Reference.PlanWithDrops(ctx, in)
}

type plainAdapter struct{ Adapter }

func TestExecuteRecordingInterface(t *testing.T) {
	in := testPlan(t)
	s := Scenario{Name: "probe", Steps: []Decision{{Plan: &in}}}
	for _, record := range []bool{false, true} {
		d := Reference{}.Declare()
		d.DropRecords = record
		p := &recordingProbe{Reference: Reference{Policy: d}}
		if _, err := Execute(t.Context(), p, s); err != nil {
			t.Fatal(err)
		}
		if record && (p.planCalls != 0 || p.dropCalls != 1) || !record && (p.planCalls != 1 || p.dropCalls != 0) {
			t.Fatalf("calls %+v", p)
		}
		if _, err := Execute(t.Context(), plainAdapter{Adapter: p}, s); (err != nil) != record {
			t.Fatalf("optional interface requirement: %v", err)
		}
	}
}

func TestRecordedAnswerValidation(t *testing.T) {
	d := Reference{}.Declare()
	d.DropRecords = true
	in := testPlan(t)
	out, _, err := (Reference{Policy: d}).PlanWithDrops(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(recordedPlanOutput{PlanOutput: out, Drops: []Drop{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"missing", "null", "duplicate", "reason", "unordered", "empty", "wrong-type"} {
		t.Run(change, func(t *testing.T) {
			var fields map[string]jsontext.Value
			_ = json.Unmarshal(valid, &fields)
			switch change {
			case "missing":
				delete(fields, "drops")
			case "null":
				fields["drops"] = jsontext.Value(`null`)
			case "wrong-type":
				fields["drops"] = jsontext.Value(`{}`)
			case "reason":
				fields["drops"] = jsontext.Value(`[{"reason":"paused_passed_over","instants":["2030-01-01T01:00:00Z"]}]`)
			case "empty":
				fields["drops"] = jsontext.Value(`[{"reason":"catchup_missed","instants":[]}]`)
			case "unordered":
				fields["drops"] = jsontext.Value(`[{"reason":"catchup_missed","instants":["2030-01-01T02:00:00Z","2030-01-01T01:00:00Z"]}]`)
			case "duplicate":
				fields["drops"] = jsontext.Value(`[{"reason":"catchup_missed","instants":["2030-01-01T01:00:00Z"]},{"reason":"catchup_missed","instants":["2030-01-01T02:00:00Z"]}]`)
			}
			raw, _ := json.Marshal(fields)
			if _, _, err := decodePlanOutput(raw, d); err == nil {
				t.Fatal("accepted malformed drops")
			}
		})
	}
	if _, _, err := decodePlanOutput(valid, Reference{}.Declare()); err == nil {
		t.Fatal("accepted undeclared drops")
	}
}
