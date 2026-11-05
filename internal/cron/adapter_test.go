package cron

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func TestAdapterScheduleProfile(t *testing.T) {
	anchor := time.Date(2030, 1, 1, 0, 0, 0, 123, time.UTC)
	for _, tc := range []struct {
		name         string
		trigger      Trigger
		cursor, want time.Time
	}{
		{"interval before creation", Trigger{Kind: "interval", IntervalSeconds: 60}, anchor.Add(-time.Hour), anchor.Add(time.Minute)},
		{"interval at creation", Trigger{Kind: "interval", IntervalSeconds: 60}, anchor, anchor.Add(time.Minute)},
		{"fractional grid", Trigger{Kind: "interval", IntervalSeconds: 60}, anchor.Add(time.Minute - time.Nanosecond), anchor.Add(time.Minute)},
		{"distant anchor", Trigger{Kind: "interval", IntervalSeconds: 60}, anchor.AddDate(500, 0, 0), anchor.AddDate(500, 0, 0).Add(time.Minute)},
		{"every effective duration", Trigger{Kind: "cron", Cron: "@every 1500ms"}, anchor, anchor.Add(time.Second)},
		{"once at creation excluded", Trigger{Kind: "once", At: anchor.Format(time.RFC3339Nano)}, anchor.Add(-time.Hour), time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := AdapterSchedule(tc.trigger, anchor)
			if err != nil {
				t.Fatal(err)
			}
			if got := next(tc.cursor); !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, trigger := range []Trigger{{Kind: "manual"}, {Kind: "interval"}, {Kind: "cron", Cron: "invalid"}} {
		if _, err := AdapterSchedule(trigger, anchor); err == nil {
			t.Errorf("accepted %+v", trigger)
		}
	}
}

func TestWorkflowStepReferences(t *testing.T) {
	for _, tc := range []struct {
		name    string
		steps   []WorkflowStep
		wantErr bool
	}{
		{"ordered", []WorkflowStep{{Key: "a"}, {Key: "b", After: []string{"a"}, Retries: 100}}, false},
		{"duplicate", []WorkflowStep{{Key: "a"}, {Key: "a"}}, true},
		{"unknown dependency", []WorkflowStep{{Key: "a", After: []string{"b"}}}, true},
		{"self dependency", []WorkflowStep{{Key: "a", After: []string{"a"}}}, true},
		{"negative retries", []WorkflowStep{{Key: "a", Retries: -1}}, true},
		{"excess retries", []WorkflowStep{{Key: "a", Retries: 101}}, true},
		{"invalid key", []WorkflowStep{{Key: "bad key"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateWorkflowStepReferences(tc.steps); (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v, got %v", tc.wantErr, err)
			}
		})
	}
}
