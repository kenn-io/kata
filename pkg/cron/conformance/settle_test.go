package conformance

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSettlementTimeline(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return start.Add(time.Duration(seconds) * time.Second) }
	offer := func(status string, a, b int) Attempt {
		end := at(b)
		if status == "running" {
			end = time.Time{}
		}
		return Attempt{StartedAt: at(a), EndedAt: end, Status: status}
	}
	for _, tc := range []struct {
		name           string
		steps          []WorkflowStep
		offers         map[string][]Attempt
		timeout        int64
		cancel         *time.Time
		certain        bool
		policy, status string
		counts         map[string]int
		skipped        []string
		occupied       bool
	}{
		{name: "retry gap before cutoff", steps: []WorkflowStep{{Key: "a", Retries: 1}}, offers: map[string][]Attempt{"a": {offer("failed", 0, 1)}}, certain: true, status: "running", counts: map[string]int{"a": 1}, occupied: true},
		{name: "retry gap after cutoff", steps: []WorkflowStep{{Key: "a", Retries: 1}}, offers: map[string][]Attempt{"a": {offer("failed", 0, 1)}}, timeout: 5, status: "failed", counts: map[string]int{"a": 1}},
		{name: "unknown holds with another failure", steps: []WorkflowStep{{Key: "a"}, {Key: "b"}}, offers: map[string][]Attempt{"a": {offer("failed", 0, 1)}, "b": {offer("unknown", 0, 2)}}, status: "unknown", counts: map[string]int{"a": 1, "b": 1}, occupied: true},
		{name: "certain cancellation outranks failure", steps: []WorkflowStep{{Key: "a"}, {Key: "b"}}, offers: map[string][]Attempt{"a": {offer("failed", 0, 1)}, "b": {offer("running", 0, 0)}}, cancel: func() *time.Time { v := at(5); return &v }(), certain: true, status: "cancelled", counts: map[string]int{"a": 1, "b": 1}},
		{name: "equal certain", steps: []WorkflowStep{{Key: "a"}}, offers: map[string][]Attempt{"a": {offer("running", 0, 0)}}, timeout: 5, cancel: func() *time.Time { v := at(5); return &v }(), certain: true, status: "cancelled", counts: map[string]int{"a": 1}, occupied: false},
		{name: "equal uncertain", steps: []WorkflowStep{{Key: "a"}}, offers: map[string][]Attempt{"a": {offer("running", 0, 0)}}, timeout: 5, cancel: func() *time.Time { v := at(5); return &v }(), certain: false, status: "unknown", counts: map[string]int{"a": 1}, occupied: true},
		{name: "stop equal-time prerequisite", steps: []WorkflowStep{{Key: "a"}, {Key: "b", After: []string{"a"}}}, offers: map[string][]Attempt{"a": {offer("succeeded", 0, 0)}, "b": {offer("failed", 0, 0)}}, policy: "stop", status: "failed", counts: map[string]int{"a": 1, "b": 1}},
		{name: "stop started independent", steps: []WorkflowStep{{Key: "a"}, {Key: "b"}}, offers: map[string][]Attempt{"a": {offer("failed", 0, 2)}, "b": {offer("succeeded", 1, 3)}}, policy: "stop", status: "failed", counts: map[string]int{"a": 1, "b": 1}},
		{name: "stop started reversed", steps: []WorkflowStep{{Key: "b"}, {Key: "a"}}, offers: map[string][]Attempt{"a": {offer("failed", 0, 2)}, "b": {offer("succeeded", 1, 3)}}, policy: "stop", status: "failed", counts: map[string]int{"a": 1, "b": 1}},
		{name: "stop unstarted independent", steps: []WorkflowStep{{Key: "a"}, {Key: "b"}}, offers: map[string][]Attempt{"a": {offer("failed", 0, 2)}, "b": {offer("succeeded", 2, 3)}}, policy: "stop", status: "failed", counts: map[string]int{"a": 1, "b": 0}, skipped: []string{"b"}},
		{name: "waiting dependency", steps: []WorkflowStep{{Key: "a"}, {Key: "b", After: []string{"a"}}}, offers: map[string][]Attempt{"a": {offer("running", 0, 0)}}, status: "running", counts: map[string]int{"a": 1, "b": 0}, occupied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Reference{}.Declare()
			if tc.policy != "" {
				d.FailurePolicy = tc.policy
			}
			out, err := (Reference{Policy: d}).Settle(t.Context(), SettleInput{Steps: tc.steps, Outcomes: tc.offers, StartedAt: start, Now: at(20), TimeoutSeconds: tc.timeout, CancelledAt: tc.cancel, StoppedWithCertainty: tc.certain})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != tc.status || out.Occupied != tc.occupied {
				t.Fatalf("got %+v", out)
			}
			for key, want := range tc.counts {
				if out.Attempts[key] != want {
					t.Errorf("%s attempts %d, want %d", key, out.Attempts[key], want)
				}
			}
			if len(out.Skipped) != len(tc.skipped) {
				t.Fatalf("skipped %v", out.Skipped)
			}
			for i, key := range tc.skipped {
				if out.Skipped[i] != key {
					t.Errorf("skipped %v", out.Skipped)
				}
			}
		})
	}
}

// Offered outcomes are consumed only after the branch policy permits a start.
// Stop must skip c after a's failure even though its offered start precedes b's
// success; the same offer is a real dependency error when it can be consumed.
func TestStopPolicyValidatesOnlyConsumedOffers(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"late-offer", "premature-terminal-tie", "premature-success-chain"} {
		for _, order := range []string{"failure-first", "failure-last"} {
			for _, policy := range []string{"stop", "continue"} {
				for _, failed := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/%s/%s/failure=%v", scenario, order, policy, failed), func(t *testing.T) {
						steps := []WorkflowStep{{Key: "a"}, {Key: "b"}, {Key: "c", After: []string{"b"}}}
						aStatus := "succeeded"
						if failed {
							aStatus = "failed"
						}
						in := SettleInput{Steps: steps, StartedAt: start, Now: start.Add(5 * time.Second), StoppedWithCertainty: true, Outcomes: map[string][]Attempt{
							"a": {{StartedAt: start, EndedAt: start.Add(time.Second), Status: aStatus}},
							"b": {{StartedAt: start, EndedAt: start.Add(3 * time.Second), Status: "succeeded"}},
							"c": {{StartedAt: start.Add(2 * time.Second), EndedAt: start.Add(4 * time.Second), Status: "succeeded"}},
						}}
						wantCounts, wantSkipped := map[string]int{"a": 1, "b": 1, "c": 0}, []string{"c"}
						// Reproduced tie and transitive failures: premature evidence cannot select stop.
						if scenario != "late-offer" {
							in.Outcomes["c"] = []Attempt{{StartedAt: start.Add(time.Second), EndedAt: start.Add(time.Second), Status: "failed"}}
						}
						if scenario == "premature-success-chain" {
							in.Outcomes["c"][0].Status = "succeeded"
							in.Outcomes["d"] = []Attempt{{StartedAt: start.Add(time.Second), EndedAt: start.Add(time.Second), Status: "failed"}}
							steps = append(steps, WorkflowStep{Key: "d", After: []string{"c"}})
							wantCounts["d"] = 0
							wantSkipped = []string{"c", "d"}
						}
						if order == "failure-last" {
							steps = append(steps[1:], steps[0])
						}
						in.Steps = steps
						d := Reference{}.Declare()
						d.FailurePolicy = policy
						out, err := (Reference{Policy: d}).Settle(t.Context(), in)
						if policy != "stop" || !failed {
							if err == nil || !strings.Contains(err.Error(), "starts before dependencies succeeded") {
								t.Fatalf("consumed premature offer must fail: %+v %v", out, err)
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						if out.Status != "failed" || out.Occupied || !reflect.DeepEqual(out.Attempts, wantCounts) || !reflect.DeepEqual(out.Skipped, wantSkipped) {
							t.Fatalf("stop consumed an excluded offer: %+v", out)
						}
					})
				}
			}
		}
	}
	// Checked contract: every genuine terminal at the earliest instant retains its result,
	// including cancellation/deadline terminals for an independent unstarted step.
	for _, kind := range []string{"failure", "mixed-duration-failure", "cancelled", "deadline"} {
		for _, reversed := range []bool{false, true} {
			t.Run(fmt.Sprintf("terminal-ties/%s/reversed=%v", kind, reversed), func(t *testing.T) {
				steps := []WorkflowStep{{Key: "a"}, {Key: "x"}}
				if reversed {
					steps[0], steps[1] = steps[1], steps[0]
				}
				in := SettleInput{Steps: steps, StartedAt: start, Now: start.Add(5 * time.Second), StoppedWithCertainty: true,
					Outcomes: map[string][]Attempt{
						"a": {{StartedAt: start, EndedAt: start, Status: "failed"}},
						"x": {{StartedAt: start, EndedAt: start, Status: "failed"}},
					}}
				wantCounts, wantStatus := map[string]int{"a": 1, "x": 1}, "failed"
				if kind == "mixed-duration-failure" {
					in.Outcomes["a"][0].EndedAt = start.Add(time.Second)
					in.Outcomes["x"][0].StartedAt = start.Add(time.Second)
					in.Outcomes["x"][0].EndedAt = start.Add(time.Second)
				} else if kind != "failure" {
					in.Outcomes = map[string][]Attempt{"a": {{StartedAt: start, Status: "running"}}}
					wantCounts["x"] = 0
					in.TimeoutSeconds = 2
					if kind == "cancelled" {
						at := start.Add(2 * time.Second)
						in.CancelledAt, wantStatus = &at, "cancelled"
					}
				}
				d := Reference{}.Declare()
				d.FailurePolicy = "stop"
				out, err := (Reference{Policy: d}).Settle(t.Context(), in)
				if err != nil || out.Status != wantStatus || out.Occupied || !reflect.DeepEqual(out.Attempts, wantCounts) || len(out.Skipped) != 0 {
					t.Fatalf("tied terminal lost: %+v %v", out, err)
				}
			})
		}
	}
}
