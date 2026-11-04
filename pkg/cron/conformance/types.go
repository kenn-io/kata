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
type Checkpoint struct {
	Through            time.Time   `json:"through"`
	DefinitionEventUID string      `json:"definition_event_uid"`
	Pending            []time.Time `json:"pending"`
}
type PlanInput struct {
	Adapter     string      `json:"adapter"`
	Job         Job         `json:"job"`
	InstalledAt time.Time   `json:"installed_at"`
	Now         time.Time   `json:"now"`
	Checkpoint  *Checkpoint `json:"checkpoint"`
	ActiveRuns  int         `json:"active_runs"`
}
type PlanOutput struct {
	Launch     []time.Time `json:"launch"`
	Checkpoint Checkpoint  `json:"checkpoint"`
	Excluded   string      `json:"excluded"`
}
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
type SettleInput struct {
	Steps                []WorkflowStep       `json:"steps"`
	Outcomes             map[string][]Attempt `json:"outcomes"`
	StartedAt            time.Time            `json:"started_at"`
	Now                  time.Time            `json:"now"`
	TimeoutSeconds       int64                `json:"timeout_seconds"`
	CancelledAt          *time.Time           `json:"cancelled_at"`
	StoppedWithCertainty bool                 `json:"stopped_with_certainty"`
}
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
type Scenario struct {
	Name  string     `json:"name"`
	Steps []Decision `json:"steps"`
}
type FixtureSuite struct {
	Schema      string      `json:"schema"`
	Declaration Declaration `json:"declaration"`
	Scenarios   []Scenario  `json:"scenarios"`
}
type Answer struct {
	Name    string           `json:"name"`
	Outputs []jsontext.Value `json:"outputs"`
}
type Failure struct {
	Name string `json:"name"`
	Diff string `json:"diff"`
}
type Report struct {
	Schema string    `json:"schema"`
	Passed int       `json:"passed"`
	Failed []Failure `json:"failed"`
}

type Adapter interface {
	Declare() Declaration
	Plan(context.Context, PlanInput) (PlanOutput, error)
	Settle(context.Context, SettleInput) (SettleOutput, error)
}
