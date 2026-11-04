// Package conformance checks the opt-in Kata cron adapter profile using offline,
// deterministic decisions. It does not launch work or certify an executor's I/O.
package conformance

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"time"
)

// Schema identifies the versioned adapter conformance fixture format.
const Schema = "kata.cron.adapter-conformance/v1"

// Declaration fixes the optional behavior tested by both the Go and CLI kits.
// Zero count/age limits mean unlimited. FailurePolicy is continue or stop.
type Declaration struct {
	Name                 string `json:"name"`
	ToleranceSeconds     int64  `json:"tolerance_seconds"`
	CatchupLimit         int    `json:"catchup_limit"`
	CatchupWindowSeconds int64  `json:"catchup_window_seconds"`
	FailurePolicy        string `json:"failure_policy"`
}

// Validate checks the declaration's name and v1 policy bounds.
func (d Declaration) Validate() error {
	if strings.TrimSpace(d.Name) == "" || d.ToleranceSeconds < 0 || d.ToleranceSeconds > 120 || d.CatchupLimit < 0 || d.CatchupLimit > 0 && d.CatchupLimit < 100 || d.CatchupWindowSeconds < 0 || d.CatchupWindowSeconds > 0 && d.CatchupWindowSeconds < 86400 || d.CatchupWindowSeconds > int64((1<<63-1)/int64(time.Second)) || d.FailurePolicy != "continue" && d.FailurePolicy != "stop" {
		return fmt.Errorf("invalid adapter declaration")
	}
	return nil
}

// Job carries full portable definition JSON without exposing internal Go types.
type Job struct {
	CreatedAt          time.Time      `json:"created_at"`
	DefinitionEventUID string         `json:"definition_event_uid"`
	Definition         jsontext.Value `json:"definition"`
	Deleted            bool           `json:"deleted"`
}

// Checkpoint records the enumeration horizon, definition revision, and pending instants.
type Checkpoint struct {
	Through            time.Time   `json:"through"`
	DefinitionEventUID string      `json:"definition_event_uid"`
	Pending            []time.Time `json:"pending"`
}

// PlanInput supplies job state, adapter identity, and local occupancy for a decision.
type PlanInput struct {
	Adapter     string      `json:"adapter"`
	Job         Job         `json:"job"`
	InstalledAt time.Time   `json:"installed_at"`
	Now         time.Time   `json:"now"`
	Checkpoint  *Checkpoint `json:"checkpoint"`
	ActiveRuns  int         `json:"active_runs"`
}

// PlanOutput contains launches and reusable state from a planning decision.
type PlanOutput struct {
	Launch     []time.Time `json:"launch"`
	Checkpoint Checkpoint  `json:"checkpoint"`
	// Excluded is empty when schedulable or one of deleted, disabled, notify,
	// manual, other_adapter, or once_past when the job is excluded.
	Excluded string `json:"excluded"`
}

// WorkflowStep describes a step and the steps that must succeed before it starts.
type WorkflowStep struct {
	Key     string   `json:"key"`
	After   []string `json:"after"`
	Retries int      `json:"retries"`
}

// Attempt offers one possible attempt's timing and outcome. Later attempts may
// not be consumed after success, unknown, exhausted retries, or a cutoff.
type Attempt struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Status    string    `json:"status"`
}

// SettleInput supplies timestamped attempt outcomes for one run.
type SettleInput struct {
	Steps          []WorkflowStep       `json:"steps"`
	Outcomes       map[string][]Attempt `json:"outcomes"`
	StartedAt      time.Time            `json:"started_at"`
	Now            time.Time            `json:"now"`
	TimeoutSeconds int64                `json:"timeout_seconds"`
	CancelledAt    *time.Time           `json:"cancelled_at"`
	// StoppedWithCertainty confirms that unknown attempts have terminated.
	// Their unknown status stays visible, but they no longer hold occupancy.
	// A certain timeout is reported as failed instead of unknown.
	StoppedWithCertainty bool `json:"stopped_with_certainty"`
}

// SettleOutput reports consumed attempts, skipped steps, status, and occupancy.
type SettleOutput struct {
	Attempts map[string]int `json:"attempts"`
	Skipped  []string       `json:"skipped"`
	Status   string         `json:"status"`
	Occupied bool           `json:"occupied"`
}

// Decision has exactly one input. Sequence flags feed actual prior output into
// this decision, so a harness cannot accidentally reset from expected state.
type Decision struct {
	Plan          *PlanInput     `json:"plan,omitempty"`
	Settle        *SettleInput   `json:"settle,omitempty"`
	UseCheckpoint bool           `json:"use_checkpoint,omitempty"`
	UseOccupancy  bool           `json:"use_occupancy,omitempty"`
	Expect        jsontext.Value `json:"expect"`
}

// Scenario contains an ordered sequence of planning or settlement decisions.
type Scenario struct {
	Name  string     `json:"name"`
	Steps []Decision `json:"steps"`
}

// FixtureSuite carries the schema, adapter declaration, and scenarios to check.
type FixtureSuite struct {
	Schema      string      `json:"schema"`
	Declaration Declaration `json:"declaration"`
	Scenarios   []Scenario  `json:"scenarios"`
}

// Answer contains an adapter's outputs for every decision in one scenario.
type Answer struct {
	Name    string           `json:"name"`
	Outputs []jsontext.Value `json:"outputs"`
}

// Failure describes one missing or mismatched scenario result.
type Failure struct {
	Name string `json:"name"`
	Diff string `json:"diff"`
}

// Report summarizes passed scenarios and per-scenario failures.
type Report struct {
	Schema string    `json:"schema"`
	Passed int       `json:"passed"`
	Failed []Failure `json:"failed"`
}

// Adapter exposes the scheduling and settlement decisions checked by this package.
type Adapter interface {
	Declare() Declaration
	Plan(context.Context, PlanInput) (PlanOutput, error)
	Settle(context.Context, SettleInput) (SettleOutput, error)
}
