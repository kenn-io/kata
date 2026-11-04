package conformance

import (
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
