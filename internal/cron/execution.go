package cron

import "go.kenn.io/kata/internal/cronpolicy"

// ExecuteMode controls which replica definitions a local adapter may execute.
type ExecuteMode = cronpolicy.ExecuteMode

const (
	// ModeNone excludes all replica jobs and workflows.
	ModeNone = cronpolicy.ModeNone
	// ModeOwned allows definitions authored by the upstream member actor.
	ModeOwned = cronpolicy.ModeOwned
	// ModeAll allows all replica jobs and workflows.
	ModeAll = cronpolicy.ModeAll
)

// ParseExecuteMode recognizes the supported local replica execution modes.
func ParseExecuteMode(value string) (ExecuteMode, bool) {
	return cronpolicy.ParseExecuteMode(value)
}

// ExecutionEligible compares the definition author with the upstream member
// identity. Unknown modes deny; owned compares exact, nonblank actor labels.
func ExecutionEligible(mode ExecuteMode, memberActor, author string) bool {
	return cronpolicy.ExecutionEligible(mode, memberActor, author)
}
